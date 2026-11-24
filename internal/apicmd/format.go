package apicmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/itchyny/gojq"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// formatFuncs are the helper functions available in plain-text output templates.
var formatFuncs = template.FuncMap{
	// time renders unix milliseconds (protojson gives int64 as a string) as
	// local wall time: 15:04:05 today, Jan 2 15:04 before.
	"time": func(v any) string {
		ms, err := toInt64(v)
		if err != nil || ms == 0 {
			return "-"
		}
		t := time.UnixMilli(ms).Local()
		if y, m, d := time.Now().Date(); t.Year() == y && t.Month() == m && t.Day() == d {
			return t.Format("15:04:05")
		}
		return t.Format("Jan 2 15:04")
	},
	// signal renders an admission signal's value against its limit:
	// "72% (holds at 60%, resumes under 30%)", "3.1 GiB free (holds under 5.0 GiB)".
	"signal": func(v any) string {
		m, _ := v.(map[string]any)
		value, _ := m["value"].(float64)
		limit, _ := m["limit"].(float64)
		resume, _ := m["resume"].(float64)
		floor, _ := m["floor"].(bool)
		unit, _ := m["unit"].(string)
		f := func(x float64) string {
			if unit == "bytes" {
				return fmt.Sprintf("%.1f GiB", x/(1<<30))
			}
			return fmt.Sprintf("%.0f%%", x)
		}
		cmp := "at"
		if floor {
			cmp = "under"
		}
		s := fmt.Sprintf("%s (holds %s %s", f(value), cmp, f(limit))
		if resume > 0 {
			s += fmt.Sprintf(", resumes under %s", f(resume))
		}
		return s + ")"
	},
	"join": func(sep string, v any) string {
		switch s := v.(type) {
		case []string:
			return strings.Join(s, sep)
		case []any:
			parts := make([]string, len(s))
			for i, item := range s {
				parts[i] = fmt.Sprint(item)
			}
			return strings.Join(parts, sep)
		default:
			return fmt.Sprint(v)
		}
	},
	// len is a list's or map's length, and a string's; 0 for a field the
	// response leaves out (protojson leaves out empty lists too).
	"len": func(v any) int {
		switch v := v.(type) {
		case []any:
			return len(v)
		case map[string]any:
			return len(v)
		case string:
			return len(v)
		}
		return 0
	},
	"trim":  strings.TrimSpace,
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	// state turns enum names like QUESTION_STATE_OPEN or MEMORY_MODE_ON_DEMAND into "open" or "on-demand".
	"state": func(v any) string {
		// A field the response leaves out (an older peer's) is nil.
		s, _ := v.(string)
		switch {
		case strings.Contains(s, "_STATE_"):
			s = s[strings.LastIndex(s, "_STATE_")+len("_STATE_"):]
		case strings.Contains(s, "_MODE_"):
			s = s[strings.LastIndex(s, "_MODE_")+len("_MODE_"):]
		case strings.Contains(s, "_SCOPE_"):
			s = s[strings.LastIndex(s, "_SCOPE_")+len("_SCOPE_"):]
		default:
			if i := strings.LastIndex(s, "_"); i >= 0 && s == strings.ToUpper(s) {
				s = s[i+1:]
			}
		}
		return strings.ToLower(strings.ReplaceAll(s, "_", "-"))
	},
	// what turns a What enum's name like WHAT_ISOLATION_LOST into
	// "isolation-lost".
	"what": func(v any) string {
		s, _ := v.(string)
		return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(s, "WHAT_"), "_", "-"))
	},
	// pad right-pads s with spaces to at least width characters.
	"pad": func(width int, v any) string {
		s := fmt.Sprint(v)
		if len(s) >= width {
			return s
		}
		return s + strings.Repeat(" ", width-len(s))
	},
	// short truncates s to at most n runes, appending "..." when trimmed.
	"short": func(n int, v any) string {
		s := strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(v)), "\n", " ")
		r := []rune(s)
		if len(r) <= n || n < 4 {
			return s
		}
		return string(r[:n-3]) + "..."
	},
	// ts formats a unix-millisecond timestamp (number or protojson string) as UTC RFC3339.
	"ts": func(v any) string {
		var ms int64
		switch x := v.(type) {
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				return x
			}
			ms = n
		case float64:
			ms = int64(x)
		case int64:
			ms = x
		case int:
			ms = int64(x)
		case json.Number:
			n, err := x.Int64()
			if err != nil {
				return x.String()
			}
			ms = n
		default:
			return fmt.Sprint(v)
		}
		if ms <= 0 {
			return "-"
		}
		return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
	},
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}

// CompileFormat parses a Go text/template for rendering a command's output.
func CompileFormat(src string) (*template.Template, error) {
	return template.New("format").Funcs(formatFuncs).Option("missingkey=zero").Parse(src)
}

// ToJSONValue turns data (a protobuf message or any Go value) into a plain
// JSON value (map[string]any, []any, string, float64, bool, nil) suitable for
// both Go templates and gojq.
func ToJSONValue(data any) (any, error) {
	if data == nil {
		return nil, nil
	}
	var raw []byte
	var err error
	if m, ok := data.(proto.Message); ok {
		raw, err = protojson.MarshalOptions{EmitDefaultValues: true}.Marshal(m)
	} else {
		raw, err = json.Marshal(data)
	}
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FormatPlain renders data to w using tmpl when non-nil, or a concise
// key/value plain-text fallback otherwise.
func FormatPlain(w io.Writer, tmpl *template.Template, data any) error {
	val, err := ToJSONValue(data)
	if err != nil {
		return err
	}
	if tmpl != nil {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, val); err != nil {
			return err
		}
		s := strings.TrimRight(buf.String(), "\n")
		if s == "" {
			return nil
		}
		_, err := fmt.Fprintln(w, s)
		return err
	}
	return formatGeneric(w, val, 0)
}

// FormatJQ evaluates query against data's JSON value and writes each result
// to w (strings raw, other values as compact JSON).
func FormatJQ(w io.Writer, query *gojq.Query, data any) error {
	val, err := ToJSONValue(data)
	if err != nil {
		return err
	}
	iter := query.Run(val)
	for {
		v, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := v.(error); isErr {
			return err
		}
		switch x := v.(type) {
		case string:
			if _, err := fmt.Fprintln(w, x); err != nil {
				return err
			}
		default:
			b, err := json.Marshal(x)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
				return err
			}
		}
	}
}

func formatGeneric(w io.Writer, val any, indent int) error {
	pad := strings.Repeat("  ", indent)
	switch v := val.(type) {
	case nil:
		return nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		maxKey := 0
		for k, fv := range v {
			if isEmptyValue(fv) {
				continue
			}
			keys = append(keys, k)
			if len(k) > maxKey {
				maxKey = len(k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			fv := v[k]
			switch child := fv.(type) {
			case map[string]any:
				if _, err := fmt.Fprintf(w, "%s%s\n", pad, k); err != nil {
					return err
				}
				if err := formatGeneric(w, child, indent+1); err != nil {
					return err
				}
			case []any:
				if allScalars(child) {
					parts := make([]string, len(child))
					for i, item := range child {
						parts[i] = formatScalar(item)
					}
					if _, err := fmt.Fprintf(w, "%s%-*s  %s\n", pad, maxKey, k, strings.Join(parts, ", ")); err != nil {
						return err
					}
				} else {
					if _, err := fmt.Fprintf(w, "%s%s\n", pad, k); err != nil {
						return err
					}
					for _, item := range child {
						if err := formatGeneric(w, item, indent+1); err != nil {
							return err
						}
					}
				}
			default:
				if _, err := fmt.Fprintf(w, "%s%-*s  %s\n", pad, maxKey, k, formatScalar(fv)); err != nil {
					return err
				}
			}
		}
		return nil
	case []any:
		for _, item := range v {
			if err := formatGeneric(w, item, indent); err != nil {
				return err
			}
		}
		return nil
	default:
		_, err := fmt.Fprintf(w, "%s%s\n", pad, formatScalar(v))
		return err
	}
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

func allScalars(xs []any) bool {
	for _, x := range xs {
		switch x.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func formatScalar(v any) string {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprint(x)
	}
}
