package api

import (
	"strconv"
	"strings"

	hotbedv1 "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// EventWhat is what a session event says happened, as a word ("exited",
// "isolation lost", "answered"), and the ID of what it happened to: a
// command's, a workspace's, a question's or a review's.
func EventWhat(e *hotbedv1.SessionEvent) (what, id string) {
	var name string
	switch ev := e.Event.(type) {
	case *hotbedv1.SessionEvent_Command:
		name, id = ev.Command.What.String(), ev.Command.CommandId
	case *hotbedv1.SessionEvent_Workspace:
		name, id = ev.Workspace.What.String(), ev.Workspace.WorkspaceId
	case *hotbedv1.SessionEvent_Question:
		name, id = ev.Question.What.String(), strconv.Itoa(int(ev.Question.Question.GetId()))
	case *hotbedv1.SessionEvent_Review:
		name, id = ev.Review.What.String(), strconv.Itoa(int(ev.Review.ReviewId))
	default:
		return "", ""
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "WHAT_")), "_", " "), id
}
