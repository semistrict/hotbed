package profiler

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler/profilertest"
)

func TestUpgradeLegacyProfile(t *testing.T) {
	p := &pb.CommandProfile{}
	if err := proto.Unmarshal(profilertest.LegacyProfile(), p); err != nil {
		t.Fatal(err)
	}
	upgraded, err := UpgradeLegacyProfile(p)
	if err != nil || !upgraded {
		t.Fatalf("UpgradeLegacyProfile = %v, %v; want true, nil", upgraded, err)
	}
	var stacks []string
	for _, st := range p.Stacks {
		stacks = append(stacks, fmt.Sprintf("%s %d %s %s", flame.Fold(p.Frames, st), st.Samples, st.LeafType, st.Comm))
	}
	if want := []string{"main.main;main.work;vfs_write_[k] 3 kernel app", "main.main;main.work;runtime.mallocgc 2 go app"}; !slices.Equal(stacks, want) {
		t.Fatalf("stacks = %q, want %q", stacks, want)
	}
	var fns []string
	for _, fn := range p.TopFunctions {
		f := p.Frames[fn.Frame]
		fns = append(fns, fmt.Sprintf("%s %s %s %s:%d %d/%d", f.Name, f.Module, f.FrameType, f.SourceFile, f.SourceLine, fn.SelfSamples, fn.TotalSamples))
	}
	if want := []string{"vfs_write vmlinux kernel :0 3/3", "main.work app go work.go:12 0/5"}; !slices.Equal(fns, want) {
		t.Fatalf("functions = %q, want %q", fns, want)
	}
	// A function's stacks name the function's frame, metadata and all.
	if p.Stacks[0].Frames[1] != p.TopFunctions[1].Frame || p.Stacks[0].Frames[2] != p.TopFunctions[0].Frame {
		t.Fatalf("stack frames %v do not name the functions' frames %d, %d", p.Stacks[0].Frames, p.TopFunctions[1].Frame, p.TopFunctions[0].Frame)
	}
	// A leaf no function names takes its stack's leaf type.
	if f := p.Frames[p.Stacks[1].Frames[2]]; f.Name != "runtime.mallocgc" || f.FrameType != "go" {
		t.Fatalf("leaf frame = %v, want runtime.mallocgc of type go", f)
	}
	if b := p.Buckets[0]; b.Samples != 5 || b.TopFrame != p.TopFunctions[0].Frame {
		t.Fatalf("bucket = %v, want 5 samples topped by vfs_write", b)
	}
	if len(p.Frames) != 4 || p.CommandId != "cmd0000001" || p.TotalSamples != 5 {
		t.Fatalf("profile = %v", p)
	}

	// Nothing of the old shape is left, and today's shape is left alone.
	again, err := UpgradeLegacyProfile(p)
	if err != nil || again {
		t.Fatalf("second UpgradeLegacyProfile = %v, %v; want false, nil", again, err)
	}
	b, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	reread := &pb.CommandProfile{}
	if err := proto.Unmarshal(b, reread); err != nil || !proto.Equal(reread, p) {
		t.Fatalf("re-read profile = %v, %v; want %v", reread, err, p)
	}
}

// A profile file a worker kept from before frames had a table loads in
// today's shape; one naming frames it lacks does not load.
func TestLoadProfileAdmitsWhatOlderBuildsStored(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "cmd0000001.pb.gz")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(profilertest.LegacyProfile())
	w.Close()
	if err := os.WriteFile(old, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfile(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := flame.Fold(p.Frames, p.Stacks[1]); got != "main.main;main.work;runtime.mallocgc" {
		t.Fatalf("second stack = %q", got)
	}

	bad := filepath.Join(dir, "cmd0000002.pb.gz")
	if err := SaveProfile(bad, &pb.CommandProfile{CommandId: "cmd0000002", Buckets: []*pb.ProfileBucket{{Samples: 1, TopFrame: 3}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(bad); err == nil || err.Error() != "profile cmd0000002: bucket 0 names frame 3 of 0" {
		t.Fatalf("LoadProfile of a profile naming frames it lacks = %v", err)
	}

	// noTopFrame is admitted even when Frames is empty, while out-of-range
	// or mismatched bucket stack references are rejected.
	if err := Admit(&pb.CommandProfile{CommandId: "cmd0000003", Buckets: []*pb.ProfileBucket{{Samples: 1, TopFrame: noTopFrame}}}); err != nil {
		t.Fatalf("Admit rejected noTopFrame: %v", err)
	}
	if err := Admit(&pb.CommandProfile{CommandId: "cmd0000004", Buckets: []*pb.ProfileBucket{{Samples: 1, TopFrame: noTopFrame, StackIndexes: []uint32{0}, StackSamples: []int64{1}}}}); err == nil {
		t.Fatalf("Admit accepted out-of-range bucket stack index")
	}
	if err := Admit(&pb.CommandProfile{CommandId: "cmd0000005", Stacks: []*pb.FoldedStack{{Samples: 1}}, Buckets: []*pb.ProfileBucket{{Samples: 1, TopFrame: noTopFrame, StackIndexes: []uint32{0}}}}); err == nil {
		t.Fatalf("Admit accepted mismatched StackIndexes/StackSamples lengths")
	}
}
