package controller

import (
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A repo stored before its freshen commands became steps still loads.
func TestStoredRecordsOutliveDroppedFields(t *testing.T) {
	r, err := unmarshal(`{"id":"hexyl","url":"https://github.com/sharkdp/hexyl","freshen":["cargo build"]}`, &pb.Repo{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Id != "hexyl" || r.Url != "https://github.com/sharkdp/hexyl" || len(r.Steps) != 0 {
		t.Errorf("repo = %v", r)
	}
}
