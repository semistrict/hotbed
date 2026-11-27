package apicmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// expr is a compiled default or fixed value expression for a protobuf field.
//
// Supported forms:
//   - "session"        the session the command acts as (string field; requires command session: true)
//   - "named_session"  the session named by -s/HOTBED_SESSION (string field; requires command session: true)
//   - "env:VAR"        the variable VAR of the Runner's Getenv (string field)
//   - literals         quoted string ('...' or "..."), bool (true/false),
//     integer, float, enum (EnumType.VALUE, VALUE, or word), or a bracketed
//     list of literals ([a, b]) for repeated fields.
type expr struct {
	eval        func(ctx context.Context, c *Call) (any, error)
	usesSession bool
}

// compile compiles a command's fixed and default expressions, each checked
// against its field's type.
func compile(sc *spec.Command) (*Command, error) {
	c := &Command{Command: sc, defaults: map[*spec.Param]expr{}}
	for _, fx := range sc.Fixed {
		e, err := compileExpr(fx.Value, fx.Field)
		if err != nil {
			return nil, fmt.Errorf("fixed %s: %w", fx.Field.Name(), err)
		}
		if e.usesSession && !sc.Session {
			return nil, fmt.Errorf("%q reads the session, but the command acts as none (session: true)", fx.Value)
		}
		c.fixed = append(c.fixed, e)
	}
	for _, p := range sc.Params {
		if p.Default == "" {
			continue
		}
		e, err := compileExpr(p.Default, p.Field())
		if err != nil {
			return nil, fmt.Errorf("default of %s: %w", p.Field().FullName(), err)
		}
		if e.usesSession && !sc.Session {
			return nil, fmt.Errorf("%q reads the session, but the command acts as none (session: true)", p.Default)
		}
		c.defaults[p] = e
	}
	return c, nil
}

// eval evaluates e for a call, giving a Go value of its field's type.
func (c *Call) eval(ctx context.Context, e expr) (any, error) {
	return e.eval(ctx, c)
}

func compileExpr(raw string, f protoreflect.FieldDescriptor) (expr, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return expr{}, fmt.Errorf("empty expression")
	}
	switch {
	case s == "session":
		if f.IsList() || f.Kind() != protoreflect.StringKind {
			return expr{}, fmt.Errorf("%q is string, not %s", raw, fieldTypeName(f))
		}
		return expr{
			usesSession: true,
			eval:        func(ctx context.Context, c *Call) (any, error) { return c.Session(ctx) },
		}, nil
	case s == "named_session":
		if f.IsList() || f.Kind() != protoreflect.StringKind {
			return expr{}, fmt.Errorf("%q is string, not %s", raw, fieldTypeName(f))
		}
		return expr{
			usesSession: true,
			eval:        func(ctx context.Context, c *Call) (any, error) { return c.NamedSession(ctx) },
		}, nil
	case strings.HasPrefix(s, "env:"):
		varName := strings.TrimPrefix(s, "env:")
		if !validEnvVar(varName) {
			return expr{}, fmt.Errorf("invalid env variable name in %q", raw)
		}
		if f.IsList() || f.Kind() != protoreflect.StringKind {
			return expr{}, fmt.Errorf("%q is string, not %s", raw, fieldTypeName(f))
		}
		return expr{
			eval: func(_ context.Context, c *Call) (any, error) {
				if c.getenv == nil {
					return nil, fmt.Errorf("%q: the runner has no Getenv to read it from", raw)
				}
				return c.getenv(varName), nil
			},
		}, nil
	}
	if f.IsList() {
		items, err := parseList(s)
		if err != nil {
			return expr{}, fmt.Errorf("%q: %w", raw, err)
		}
		vals := make([]any, len(items))
		for i, item := range items {
			v, err := compileScalarLiteral(item, f)
			if err != nil {
				return expr{}, err
			}
			vals[i] = v
		}
		return expr{eval: func(context.Context, *Call) (any, error) { return vals, nil }}, nil
	}
	v, err := compileScalarLiteral(s, f)
	if err != nil {
		return expr{}, err
	}
	return expr{eval: func(context.Context, *Call) (any, error) { return v, nil }}, nil
}

func compileScalarLiteral(s string, f protoreflect.FieldDescriptor) (any, error) {
	switch f.Kind() {
	case protoreflect.StringKind:
		if v, ok, err := parseQuotedString(s); ok {
			return v, err
		}
		if kind := guessLiteralKind(s); kind != "" {
			return nil, fmt.Errorf("%q is %s, not string", s, kind)
		}
		return nil, fmt.Errorf("invalid string default %q (quote literals like 'value', or use session, named_session, or env:VAR)", s)
	case protoreflect.BoolKind:
		switch s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not bool", s)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not int32: %w", s, err)
		}
		return int32(n), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not int64: %w", s, err)
		}
		return n, nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not uint32: %w", s, err)
		}
		return uint32(n), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not uint64: %w", s, err)
		}
		return n, nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not float: %w", s, err)
		}
		return x, nil
	case protoreflect.EnumKind:
		name := s
		if prefix, rest, ok := strings.Cut(s, "."); ok {
			if prefix != string(f.Enum().Name()) {
				return nil, fmt.Errorf("%q: unknown enum %q, want %s", s, prefix, f.Enum().Name())
			}
			name = rest
		}
		return enumNumber(f.Enum(), name)
	default:
		return nil, fmt.Errorf("%s does not support literal defaults", f.Kind())
	}
}

func parseQuotedString(s string) (string, bool, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		v, err := strconv.Unquote(s)
		return v, true, err
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		var b strings.Builder
		inner := s[1 : len(s)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case '\'', '\\':
					b.WriteByte(inner[i])
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					return "", true, fmt.Errorf("unknown escape \\%c in %q", inner[i], s)
				}
				continue
			}
			if inner[i] == '\'' {
				return "", true, fmt.Errorf("unescaped quote in %q", s)
			}
			b.WriteByte(inner[i])
		}
		return b.String(), true, nil
	}
	return "", false, nil
}

func parseList(s string) ([]string, error) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("want a list like [a, b]")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if quote != 0 {
			cur.WriteByte(ch)
			if ch == '\\' && i+1 < len(inner) {
				i++
				cur.WriteByte(inner[i])
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			cur.WriteByte(ch)
		case ',':
			item := strings.TrimSpace(cur.String())
			if item == "" {
				return nil, fmt.Errorf("empty list item")
			}
			out = append(out, item)
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated string in list")
	}
	item := strings.TrimSpace(cur.String())
	if item == "" {
		return nil, fmt.Errorf("empty list item")
	}
	out = append(out, item)
	return out, nil
}

func validEnvVar(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func guessLiteralKind(s string) string {
	if s == "true" || s == "false" {
		return "bool"
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return "int"
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return "float"
	}
	return ""
}

func fieldTypeName(f protoreflect.FieldDescriptor) string {
	name := f.Kind().String()
	if f.Kind() == protoreflect.EnumKind {
		name = string(f.Enum().Name())
	} else if f.Message() != nil {
		name = string(f.Message().Name())
	}
	if f.IsList() {
		return "list(" + name + ")"
	}
	return name
}
