package profiler

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The cases of testdata/conformance/profile_query.json, which cf-controller's
// profile-query.ts runs too (cf-controller/test/conformance.test.ts): a
// finished command's profile, queried on the controller, must read as the
// worker's live one does.

type conformanceFrame struct {
	Name       string `json:"name"`
	Module     string `json:"module"`
	FrameType  string `json:"frameType"`
	SourceFile string `json:"sourceFile"`
	SourceLine int32  `json:"sourceLine"`
}

type conformanceFunction struct {
	Frame        string  `json:"frame"`
	SelfSamples  int64   `json:"selfSamples"`
	TotalSamples int64   `json:"totalSamples"`
	SelfPct      float64 `json:"selfPct"`
	TotalPct     float64 `json:"totalPct"`
}

type conformanceBucket struct {
	T            int64    `json:"t"`
	Samples      int64    `json:"samples"`
	TopFrame     *string  `json:"topFrame"`
	StackIndexes []uint32 `json:"stackIndexes"`
	StackSamples []int64  `json:"stackSamples"`
}

// profileSummary is a query's reply as the fixture's `want` states it: what
// the Go and TypeScript queries must both give.
type profileSummary struct {
	TotalSamples   int64                 `json:"totalSamples"`
	MatchedSamples int64                 `json:"matchedSamples"`
	KernelSamples  int64                 `json:"kernelSamples"`
	UserSamples    int64                 `json:"userSamples"`
	Frames         []conformanceFrame    `json:"frames"`
	Folded         []string              `json:"folded"`
	TopFunctions   []conformanceFunction `json:"topFunctions"`
	Buckets        []conformanceBucket   `json:"buckets"`
}

func summarizeProfile(p *pb.CommandProfile) profileSummary {
	s := profileSummary{
		TotalSamples:   p.TotalSamples,
		MatchedSamples: p.MatchedSamples,
		KernelSamples:  p.KernelSamples,
		UserSamples:    p.UserSamples,
		Frames:         []conformanceFrame{},
		Folded:         []string{},
		TopFunctions:   []conformanceFunction{},
		Buckets:        []conformanceBucket{},
	}
	for _, f := range p.Frames {
		s.Frames = append(s.Frames, conformanceFrame{f.Name, f.Module, f.FrameType, f.SourceFile, f.SourceLine})
	}
	for _, st := range p.Stacks {
		s.Folded = append(s.Folded, fmt.Sprintf("%s %d %s %s", flame.Fold(p.Frames, st), st.Samples, st.LeafType, st.Comm))
	}
	for _, fn := range p.TopFunctions {
		s.TopFunctions = append(s.TopFunctions, conformanceFunction{flame.Label(p.Frames[fn.Frame]), fn.SelfSamples, fn.TotalSamples, fn.SelfPct, fn.TotalPct})
	}
	for _, b := range p.Buckets {
		cb := conformanceBucket{T: b.T, Samples: b.Samples, StackIndexes: []uint32{}, StackSamples: []int64{}}
		if int(b.TopFrame) < len(p.Frames) {
			label := flame.Label(p.Frames[b.TopFrame])
			cb.TopFrame = &label
		}
		cb.StackIndexes = append(cb.StackIndexes, b.StackIndexes...)
		cb.StackSamples = append(cb.StackSamples, b.StackSamples...)
		s.Buckets = append(s.Buckets, cb)
	}
	return s
}

func TestQueryProfileConformance(t *testing.T) {
	data, err := os.ReadFile("../../testdata/conformance/profile_query.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile json.RawMessage `json:"profile"`
		Queries []struct {
			Name    string          `json:"name"`
			Request json.RawMessage `json:"request"`
			Want    json.RawMessage `json:"want"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Queries) == 0 {
		t.Fatal("profile_query.json has no queries")
	}
	for _, q := range fixture.Queries {
		t.Run(q.Name, func(t *testing.T) {
			// Each query reads the profile afresh: it must not change it.
			var raw pb.CommandProfile
			if err := protojson.Unmarshal(fixture.Profile, &raw); err != nil {
				t.Fatal(err)
			}
			var req pb.GetProfileRequest
			if err := protojson.Unmarshal(q.Request, &req); err != nil {
				t.Fatal(err)
			}
			got := summarizeProfile(QueryProfile(&raw, &req))

			var wantTree, gotTree any
			if err := json.Unmarshal(q.Want, &wantTree); err != nil {
				t.Fatal(err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(gotJSON, &gotTree); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotTree, wantTree) {
				pretty, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("QueryProfile(%s) =\n%s\nwant\n%s", q.Request, pretty, q.Want)
			}

			var again pb.CommandProfile
			if err := protojson.Unmarshal(fixture.Profile, &again); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(&raw, &again) {
				t.Errorf("QueryProfile changed the profile it queried")
			}
		})
	}
}
