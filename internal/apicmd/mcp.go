package apicmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// A command's MCP tool takes, as its parameters, what the command line
// gives as flags and arguments (by the same names, dashes as underscores,
// an enum by its words) and, for what the command line gives as JSON, that
// JSON: a message param is its protobuf JSON. The session is never one: the
// MCP server is the session.

// Tool is a command's MCP tool.
type Tool struct {
	Command     *Command
	Name        string
	Description string
	// InputSchema is the JSON Schema of its parameters.
	InputSchema map[string]any
	ReadOnly    bool
	Destructive bool
}

// Tools is the schema's commands' tools.
func (s *Schema) Tools() []Tool {
	var out []Tool
	for _, c := range s.Commands {
		out = append(out, Tool{
			Command: c, Name: c.Tool(), Description: c.Help, InputSchema: inputSchema(c),
			ReadOnly: c.ReadOnly, Destructive: c.Destructive,
		})
	}
	return out
}

func inputSchema(c *Command) map[string]any {
	props := map[string]any{}
	var required []string
	for _, p := range c.Params {
		if p.Hidden {
			continue
		}
		s := fieldSchema(p.Field(), 0)
		desc := p.Help
		if p.Every != "" {
			desc += fmt.Sprintf(" %q is every one.", p.Every)
			s["items"].(map[string]any)["enum"] = append(s["items"].(map[string]any)["enum"].([]string), p.Every)
		}
		if desc != "" {
			s["description"] = desc
		}
		props[p.Var()] = s
		if (p.Positional && !p.Field().IsList() && !p.Optional) || p.Required {
			required = append(required, p.Var())
		}
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// fieldSchema is the JSON Schema of a field's value, as its tool takes it:
// an enum's words, a message's protobuf JSON.
func fieldSchema(f protoreflect.FieldDescriptor, depth int) map[string]any {
	var s map[string]any
	switch f.Kind() {
	case protoreflect.BoolKind:
		s = map[string]any{"type": "boolean"}
	case protoreflect.StringKind, protoreflect.BytesKind:
		s = map[string]any{"type": "string"}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		s = map[string]any{"type": "number"}
	case protoreflect.EnumKind:
		var words []string
		for _, w := range spec.EnumWords(f.Enum()) {
			words = append(words, w.Word)
		}
		s = map[string]any{"type": "string", "enum": words}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		s = messageSchema(f.Message(), depth+1)
	default:
		s = map[string]any{"type": "integer"}
	}
	if f.IsList() {
		return map[string]any{"type": "array", "items": s}
	}
	return s
}

// messageSchema is a message's protobuf JSON's schema, its fields by their
// JSON names and enums by their names, as protojson reads them.
func messageSchema(m protoreflect.MessageDescriptor, depth int) map[string]any {
	if m.ParentFile().Package() == "google.protobuf" {
		// A well-known type has a JSON of its own (a Timestamp is a string).
		return map[string]any{"description": "protobuf JSON of " + string(m.FullName())}
	}
	if depth > 8 {
		return map[string]any{"type": "object"}
	}
	props := map[string]any{}
	for i := range m.Fields().Len() {
		f := m.Fields().Get(i)
		var s map[string]any
		if f.Enum() != nil {
			var names []string
			for j := range f.Enum().Values().Len() {
				names = append(names, string(f.Enum().Values().Get(j).Name()))
			}
			s = map[string]any{"type": "string", "enum": names}
			if f.IsList() {
				s = map[string]any{"type": "array", "items": s}
			}
		} else {
			s = fieldSchema(f, depth)
		}
		props[f.JSONName()] = s
	}
	return map[string]any{"type": "object", "properties": props}
}

// MCPArgs reads a tool's arguments as the command's.
func (c *Command) MCPArgs(raw json.RawMessage) (Args, error) {
	a := Args{Given: map[*spec.Param]any{}}
	var in map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return a, api.Errorf(api.CodeInvalidArgs, "read the arguments: %v", err)
		}
	}
	known := map[string]bool{}
	// The JSON the command line takes: the message params' JSON.
	input := map[string]json.RawMessage{}
	for _, p := range c.Params {
		if p.Hidden {
			continue
		}
		known[p.Var()] = true
		v, ok := in[p.Var()]
		if !ok {
			continue
		}
		if p.Message() {
			if c.Input != nil && c.Input.Field == p.Field() {
				a.Input = v
			} else {
				input[p.Field().JSONName()] = v
			}
			continue
		}
		val, err := jsonValue(p, v)
		if err != nil {
			return a, api.Errorf(api.CodeInvalidArgs, "%s: %v", p.Var(), err)
		}
		a.Given[p] = val
	}
	var unknown []string
	for k := range in {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return a, api.Errorf(api.CodeInvalidArgs, "%s takes no %v", c.Tool(), unknown)
	}
	if c.Input != nil && c.Input.Field == nil {
		b, err := json.Marshal(input)
		if err != nil {
			return a, err
		}
		a.Input = b
	}
	if c.Input != nil && a.Input == nil {
		return a, api.Errorf(api.CodeInvalidArgs, "%s needs %s", c.Tool(), c.Input.Field.JSONName())
	}
	return a, nil
}

// jsonValue is a param's JSON value as the Go value the command line gives.
func jsonValue(p *spec.Param, v json.RawMessage) (any, error) {
	f := p.Field()
	one := func(raw json.RawMessage) (any, error) {
		switch f.Kind() {
		case protoreflect.BoolKind:
			var b bool
			return b, json.Unmarshal(raw, &b)
		case protoreflect.StringKind, protoreflect.EnumKind:
			var s string
			return s, json.Unmarshal(raw, &s)
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			var x float64
			return x, json.Unmarshal(raw, &x)
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			var n json.Number
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, err
			}
			if u, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
				return u, nil
			}
			return n.Float64() // a whole number written 3.0; set checks it is whole
		default:
			var n json.Number
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, err
			}
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
			return n.Float64()
		}
	}
	if !f.IsList() {
		return one(v)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil {
		return nil, err
	}
	if f.Kind() == protoreflect.EnumKind {
		var out []string
		for _, it := range items {
			var s string
			if err := json.Unmarshal(it, &s); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	}
	var out []any
	for _, it := range items {
		x, err := one(it)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}
