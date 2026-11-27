package flame

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"google.golang.org/protobuf/testing/protocmp"
)

func key(ids ...uint32) string {
	var k []byte
	for _, id := range ids {
		k = AppendID(k, id)
	}
	return string(k)
}

// Folded stacks come most sampled first, ties by frames then process.
func TestStacksFolded(t *testing.T) {
	s := Stacks{}
	s[StackKey{Frames: key(0, 2), LeafType: "native", Comm: "b"}] += 3
	s[StackKey{Frames: key(0, 1), LeafType: "kernel", Comm: "a"}] += 5
	s[StackKey{Frames: key(0, 2), LeafType: "native", Comm: "a"}] += 3
	want := []*pb.FoldedStack{
		{Frames: []uint32{0, 1}, Samples: 5, LeafType: "kernel", Comm: "a"},
		{Frames: []uint32{0, 2}, Samples: 3, LeafType: "native", Comm: "a"},
		{Frames: []uint32{0, 2}, Samples: 3, LeafType: "native", Comm: "b"},
	}
	if diff := cmp.Diff(want, s.Folded(), protocmp.Transform()); diff != "" {
		t.Fatalf("Folded (-want +got):\n%s", diff)
	}
}

// A function's total counts each stack it is in once, recursion or not;
// its self, the stacks it is the leaf of.
func TestFunctionsTop(t *testing.T) {
	frames := []*pb.ProfileFrame{{Name: "main"}, {Name: "rec"}, {Name: "leaf"}}
	var f Functions
	f.Add([]uint32{0, 1, 1, 2}, 2)
	f.Add([]uint32{0, 1}, 1)
	f.Add([]uint32{0}, 1)
	want := []*pb.ProfileFunction{
		{Frame: 2, SelfSamples: 2, TotalSamples: 2, SelfPct: 50, TotalPct: 50},
		{Frame: 0, SelfSamples: 1, TotalSamples: 4, SelfPct: 25, TotalPct: 100},
		{Frame: 1, SelfSamples: 1, TotalSamples: 3, SelfPct: 25, TotalPct: 75},
	}
	if diff := cmp.Diff(want, f.Top(frames, 4), protocmp.Transform()); diff != "" {
		t.Fatalf("Top (-want +got):\n%s", diff)
	}
	var none Functions
	if got := none.Top(frames, 0); len(got) != 0 {
		t.Fatalf("empty Top = %v", got)
	}
}
