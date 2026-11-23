package flame

import (
	"slices"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestTableListsEachFrameOnce(t *testing.T) {
	var tab Table
	main := &pb.ProfileFrame{Name: "main", Module: "app", FrameType: "go"}
	ids := []uint32{
		tab.ID(main),
		tab.ID(&pb.ProfileFrame{Name: "vfs_write", FrameType: "kernel"}),
		tab.ID(&pb.ProfileFrame{Name: "main", Module: "app", FrameType: "go"}),
		tab.ID(&pb.ProfileFrame{Name: "main", Module: "app", FrameType: "go", SourceLine: 7}),
	}
	// A function counts once whatever line it was at, as first seen.
	if want := []uint32{0, 1, 0, 0}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if len(tab.Frames()) != 2 || tab.Frames()[0] == main || tab.Frames()[0].SourceLine != 0 {
		t.Fatalf("frames = %v, want copies of the first two", tab.Frames())
	}
	// A process at a stack's root is not the function of its name.
	if id := tab.ID(&pb.ProfileFrame{Name: "main", FrameType: "process"}); id != 2 {
		t.Fatalf("process frame id = %d, want 2", id)
	}
	if id, ok := tab.Lookup("vfs_write", "kernel"); !ok || id != 1 {
		t.Fatalf("Lookup = %d, %v; want 1, true", id, ok)
	}
}

func TestRebaseAndFold(t *testing.T) {
	from := []*pb.ProfileFrame{
		{Name: "unused"},
		{Name: "a;b", FrameType: "go"},
		{Name: "vfs_write", FrameType: "kernel"},
		{Name: ""},
	}
	stacks := []*pb.FoldedStack{{Frames: []uint32{1, 2}}, {Frames: []uint32{3, 1}}}
	fns := []*pb.ProfileFunction{{Frame: 2}}
	var tab Table
	tab.Rebase(from, stacks, fns)
	var got []string
	for _, st := range stacks {
		got = append(got, Fold(tab.Frames(), st))
	}
	if want := []string{"a:b;vfs_write_[k]", "[unknown];a:b"}; !slices.Equal(got, want) {
		t.Fatalf("folded = %q, want %q", got, want)
	}
	if fns[0].Frame != 1 || len(tab.Frames()) != 3 {
		t.Fatalf("function frame %d of %d frames, want 1 of 3", fns[0].Frame, len(tab.Frames()))
	}
}

func TestKeysAndChecks(t *testing.T) {
	ids := []uint32{0, 7, 1 << 20}
	var key []byte
	for _, id := range ids {
		key = AppendID(key, id)
	}
	if got := IDs(string(key)); !slices.Equal(got, ids) {
		t.Fatalf("IDs = %v, want %v", got, ids)
	}

	frames := []*pb.ProfileFrame{{Name: "a"}, {Name: "b"}}
	ok := []*pb.FoldedStack{{Frames: []uint32{0, 1}}}
	if err := Check(frames, ok, []*pb.ProfileFunction{{Frame: 1}}, 0); err != nil {
		t.Fatalf("Check of good indexes = %v", err)
	}
	for _, tc := range []struct {
		stacks []*pb.FoldedStack
		fns    []*pb.ProfileFunction
		tops   []uint32
		want   string
	}{
		{stacks: []*pb.FoldedStack{{Frames: []uint32{0, 2}}}, want: "stack 0 names frame 2 of 2"},
		{fns: []*pb.ProfileFunction{{Frame: 5}}, want: "function 0 names frame 5 of 2"},
		{tops: []uint32{1, 2}, want: "bucket 1 names frame 2 of 2"},
	} {
		if err := Check(frames, tc.stacks, tc.fns, tc.tops...); err == nil || err.Error() != tc.want {
			t.Errorf("Check = %v, want %q", err, tc.want)
		}
	}
}
