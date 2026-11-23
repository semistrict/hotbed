package controller

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// pollReminder is what every question call reminds the agent of: the user
// answers whenever they like, and an answer reaches the asking session only
// through its poll.
const pollReminder = "Reminder: the user answers questions in the cockpit whenever they like, and answers reach you only through `hotbed poll` (MCP: `poll`). " +
	"Before you end your turn, commit, or conclude the user has not answered, run `hotbed poll`; " +
	"act on each answer it returns, then `hotbed question ack ID`."

// remindToPoll sets the reminder of every QuestionService response (each
// has one: TestEveryQuestionResponseReminds).
func remindToPoll(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if !strings.HasPrefix(spec.Procedure, "/"+hotbedv1connect.QuestionServiceName+"/") {
			return next(ctx, spec, stream)
		}
		return next(ctx, spec, reminding{stream})
	}
}

type reminding struct{ connect.ServerStream }

func (s reminding) Send(msg any) error {
	if m, ok := msg.(proto.Message); ok {
		r := m.ProtoReflect()
		if f := r.Descriptor().Fields().ByName("reminder"); f != nil {
			r.Set(f, protoreflect.ValueOfString(pollReminder))
		}
	}
	return s.ServerStream.Send(msg)
}
