package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// typeSessionEvents (migration 15) rewrites each session event from the one
// flat shape every kind shared, told apart by its what, to its kind's own
// message: a command's, a workspace's, a question's (with the question as
// it is now) or a review's. An event of a kind no longer told is dropped.
func typeSessionEvents(ctx context.Context, tx *sql.Tx) error {
	type flat struct {
		At          json.Number `json:"at"`
		CommandID   string      `json:"commandId"`
		Argv        string      `json:"argv"`
		What        string      `json:"what"`
		State       string      `json:"state"`
		ExitCode    *int32      `json:"exitCode"`
		Detail      string      `json:"detail"`
		SessionID   string      `json:"sessionId"`
		WorkspaceID string      `json:"workspaceId"`
		QuestionID  int32       `json:"questionId"`
		ReviewID    int32       `json:"reviewId"`
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, doc FROM session_events ORDER BY seq`)
	if err != nil {
		return err
	}
	type row struct {
		seq int64
		old flat
	}
	var all []row
	for rows.Next() {
		var r row
		var doc string
		if err := rows.Scan(&r.seq, &doc); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(doc), &r.old); err != nil {
			rows.Close()
			return fmt.Errorf("decode session event %d: %w", r.seq, err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	commands := map[string]pb.CommandUpdate_What{
		"admitted": pb.CommandUpdate_WHAT_ADMITTED, "parked": pb.CommandUpdate_WHAT_PARKED, "thawed": pb.CommandUpdate_WHAT_THAWED,
		"paused": pb.CommandUpdate_WHAT_PAUSED, "resumed": pb.CommandUpdate_WHAT_RESUMED, "isolation lost": pb.CommandUpdate_WHAT_ISOLATION_LOST,
		"exited": pb.CommandUpdate_WHAT_EXITED, "cancelled": pb.CommandUpdate_WHAT_CANCELLED,
	}
	workspaces := map[string]pb.WorkspaceUpdate_What{"ready": pb.WorkspaceUpdate_WHAT_READY, "failed": pb.WorkspaceUpdate_WHAT_FAILED}
	questions := map[string]pb.QuestionUpdate_What{"answered": pb.QuestionUpdate_WHAT_ANSWERED, "dismissed": pb.QuestionUpdate_WHAT_DISMISSED}
	reviews := map[string]pb.ReviewUpdate_What{
		"reviewed": pb.ReviewUpdate_WHAT_FINISHED, "review failed": pb.ReviewUpdate_WHAT_FAILED, "responded": pb.ReviewUpdate_WHAT_RESPONDED,
	}
	for _, r := range all {
		o := r.old
		at, err := strconv.ParseInt(o.At.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("session event %d's at: %w", r.seq, err)
		}
		e := &pb.SessionEvent{At: at, SessionId: o.SessionID, Detail: o.Detail}
		switch {
		case o.CommandID != "":
			w, ok := commands[o.What]
			if !ok {
				break
			}
			e.Event = &pb.SessionEvent_Command{Command: &pb.CommandUpdate{
				What: w, CommandId: o.CommandID, Argv: o.Argv, WorkspaceId: o.WorkspaceID,
				State: pb.CommandState(pb.CommandState_value[o.State]), ExitCode: o.ExitCode,
			}}
		case o.QuestionID != 0:
			w, ok := questions[o.What]
			if !ok {
				break
			}
			q := &pb.Question{Id: o.QuestionID}
			var doc string
			err := tx.QueryRowContext(ctx, `SELECT doc FROM questions WHERE id = ?`, o.QuestionID).Scan(&doc)
			if err == nil {
				if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(doc), q); err != nil {
					return fmt.Errorf("decode question %d: %w", o.QuestionID, err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			e.Event = &pb.SessionEvent_Question{Question: &pb.QuestionUpdate{What: w, Question: q}}
		case o.ReviewID != 0:
			if w, ok := reviews[o.What]; ok {
				e.Event = &pb.SessionEvent_Review{Review: &pb.ReviewUpdate{What: w, ReviewId: o.ReviewID}}
			}
		case o.WorkspaceID != "":
			if w, ok := workspaces[o.What]; ok {
				e.Event = &pb.SessionEvent_Workspace{Workspace: &pb.WorkspaceUpdate{What: w, WorkspaceId: o.WorkspaceID}}
			}
		}
		if e.Event == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM session_events WHERE seq = ?`, r.seq); err != nil {
				return err
			}
			continue
		}
		doc, err := protojson.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_events SET doc = ? WHERE seq = ?`, string(doc), r.seq); err != nil {
			return err
		}
	}
	return nil
}
