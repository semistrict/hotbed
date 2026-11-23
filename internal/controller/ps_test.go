package controller_test

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Asked for no limit, ListCommands lists the newest 50 ended commands and
// says how many more there are; any caller gets that, not only the CLI
// (board 7682).
func TestListCommandsLimitsEndedCommandsByDefault(t *testing.T) {
	s := newStreamed(t)
	if _, err := s.sessions.StartSession(t.Context(), &pb.StartSessionRequest{Id: "calm-owl", Remote: "github.com/o/app"}); err != nil {
		t.Fatal(err)
	}
	s.push(&pb.PushEventsRequest{Epoch: "e1"})
	s.push(&pb.PushEventsRequest{Epoch: "e1", Checkpoint: &pb.Checkpoint{}})
	var events []*pb.Event
	for i := range 52 {
		id := fmt.Sprintf("cmd%02d", i)
		exited := &pb.Command{Id: id, NodeId: asahi.NodeID, SessionId: "calm-owl", Argv: "make", State: pb.CommandState_COMMAND_STATE_SUCCEEDED}
		finished := &pb.FinishedCommand{Id: id, NodeId: asahi.NodeID, SessionId: "calm-owl", Argv: "make", State: pb.CommandState_COMMAND_STATE_SUCCEEDED, EndedAt: int64(1_000_000 + i)}
		events = append(events,
			cmdEvent(int64(len(events)+1), "exited", proto.Clone(exited).(*pb.Command), finished),
			cmdEvent(int64(len(events)+2), "forgotten", exited, nil))
	}
	if res := s.push(&pb.PushEventsRequest{Epoch: "e1", Events: events}); res.Applied != int64(len(events)) {
		t.Fatalf("push = %v, want applied %d", res, len(events))
	}

	ps, err := s.workspaces.ListCommands(t.Context(), &pb.ListCommandsRequest{SessionId: "calm-owl", Ended: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Commands) != 50 || ps.More != 2 || ps.Commands[0].Id != "cmd02" || ps.Commands[49].Id != "cmd51" {
		t.Errorf("ps --ended = %d commands (%v … %v), %d more; want cmd02 … cmd51 and 2 more", len(ps.Commands), ps.Commands[0].GetId(), ps.Commands[len(ps.Commands)-1].GetId(), ps.More)
	}
}
