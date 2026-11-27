package settle

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The cases of testdata/conformance/settle.json, which cf-controller's
// settle.ts runs too (cf-controller/test/conformance.test.ts): the worker
// settles live output with this package and the controller settles stored
// output with that one, so both must give the same lines for the same bytes.

// text is a string, or an array of parts (each a string or {"repeat", "count"})
// joined.
type text string

func (t *text) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*t = text(s)
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("a text is a string or an array of parts: %w", err)
	}
	var b strings.Builder
	for _, raw := range parts {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			b.WriteString(s)
			continue
		}
		var rep struct {
			Repeat string `json:"repeat"`
			Count  int    `json:"count"`
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			return fmt.Errorf("a part is a string or {repeat, count}: %w", err)
		}
		b.WriteString(strings.Repeat(rep.Repeat, rep.Count))
	}
	*t = text(b.String())
	return nil
}

// wantLine is one line of a case's output, or Times identical ones.
type wantLine struct {
	Line  text
	Times int
}

func (w *wantLine) UnmarshalJSON(data []byte) error {
	var obj struct {
		Line  text `json:"line"`
		Times int  `json:"times"`
	}
	if json.Unmarshal(data, &obj) == nil && obj.Times > 0 {
		*w = wantLine{obj.Line, obj.Times}
		return nil
	}
	w.Times = 1
	return w.Line.UnmarshalJSON(data)
}

type conformanceCase struct {
	Name  string     `json:"name"`
	Input text       `json:"input"`
	Want  []wantLine `json:"want"`
}

func (c conformanceCase) want() []string {
	var out []string
	for _, w := range c.Want {
		for range w.Times {
			out = append(out, string(w.Line))
		}
	}
	return out
}

// splitWriteLimit is the size under which a case is also fed split at every
// byte offset.
const splitWriteLimit = 512

func TestConformance(t *testing.T) {
	data, err := os.ReadFile("../../testdata/conformance/settle.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []conformanceCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("settle.json has no cases")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			in, want := []byte(tc.Input), tc.want()
			if got := Lines(in); !slices.Equal(got, want) {
				t.Fatalf("Lines(%s) = %s, want %s", abbreviate("%q", string(in)), abbreviate("%q", got), abbreviate("%q", want))
			}
			if len(in) >= splitWriteLimit {
				return
			}
			for cut := 1; cut < len(in); cut++ {
				var got []string
				s := New(func(l string) { got = append(got, l) })
				s.Write(in[:cut])
				s.Write(in[cut:])
				s.Flush()
				if !slices.Equal(got, want) {
					t.Fatalf("written split at %d, %q = %q, want %q", cut, in, got, want)
				}
			}
		})
	}
}

// abbreviate formats v with format, shortened for an error message.
func abbreviate(format string, v any) string {
	const max = 300
	s := fmt.Sprintf(format, v)
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:max], len(s))
}
