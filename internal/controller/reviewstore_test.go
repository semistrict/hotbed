package controller

import (
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Every quick response has words people use, in the line a requesting
// session reads.
func TestResponseOnSaysEveryQuickResponse(t *testing.T) {
	for q, want := range map[pb.QuickResponse]string{
		pb.QuickResponse_QUICK_RESPONSE_VERIFY:          "the user on finding 2: verify",
		pb.QuickResponse_QUICK_RESPONSE_FIX_IF_VERIFIED: "the user on finding 2: fix if verified",
		pb.QuickResponse_QUICK_RESPONSE_WONT_FIX:        "the user on finding 2: won't fix",
	} {
		if got := responseOn("finding 2", &pb.ReviewEvent{By: &pb.Stamp{}, Quick: q}); got != want {
			t.Errorf("%v: %q, want %q", q, got, want)
		}
	}
	for v := range pb.QuickResponse_name {
		if q := pb.QuickResponse(v); q != pb.QuickResponse_QUICK_RESPONSE_UNSPECIFIED && quickWords[q] == "" {
			t.Errorf("%v has no words", q)
		}
	}
	got := responseOn("review 1", &pb.ReviewEvent{By: &pb.Stamp{Session: "brave-otter"}, Text: "thanks\nmore"})
	if want := `brave-otter on review 1: "thanks"`; got != want {
		t.Errorf("a comment: %q, want %q", got, want)
	}
}
