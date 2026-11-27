package profiler

import (
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A stored profile naming frames it lacks does not load.
func TestLoadProfileRejectsMissingFrames(t *testing.T) {
	bad := t.TempDir() + "/cmd0000002.pb.gz"
	if err := SaveProfile(bad, &pb.CommandProfile{CommandId: "cmd0000002", Buckets: []*pb.ProfileBucket{{Samples: 1, TopFrame: 3}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(bad); err == nil || err.Error() != "profile cmd0000002: bucket 0 names frame 3 of 0" {
		t.Fatalf("LoadProfile of a profile naming frames it lacks = %v", err)
	}
}

// Admit validates bucket frame and stack references.
func TestAdmitValidatesBuckets(t *testing.T) {
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
