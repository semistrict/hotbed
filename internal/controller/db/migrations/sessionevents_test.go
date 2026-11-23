package migrations

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	_ "modernc.org/sqlite"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Session events stored in the flat shape every kind shared become their
// kind's own message, keeping their seq; a question's carries the question
// as stored, and an event no kind tells is dropped.
func TestTypeSessionEvents(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "c.db")+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	provider, err := newProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO projects (id, name, created_at) VALUES (1, 'test', 1)`,
		`INSERT INTO questions (id, project_id, repo, state, rank, asked_by, asked_at, updated_at, doc)
		 VALUES (4, 1, '', 'QUESTION_STATE_ANSWERED', 1, 'brave-otter', 1, 2, '{"id":4,"state":"QUESTION_STATE_ANSWERED","answer":{"picked":["SQLite"]}}')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	old := []string{
		`{"at":"10","commandId":"c1","argv":"go test","what":"exited","state":"COMMAND_STATE_FAILED","exitCode":3,"sessionId":"brave-otter","workspaceId":"w1"}`,
		`{"at":"11","what":"ready","detail":"forked","sessionId":"brave-otter","workspaceId":"w1"}`,
		`{"at":"12","what":"answered","detail":"question 4 (Store) answered: SQLite","sessionId":"brave-otter","questionId":4}`,
		`{"at":"13","what":"review failed","detail":"review 7 (X) failed","sessionId":"brave-otter","reviewId":7}`,
		`{"at":"14","commandId":"c1","what":"queued","sessionId":"brave-otter"}`,
		`{"at":"15","what":"responded","detail":"the user on review 7: ok","sessionId":"brave-otter","reviewId":7}`,
	}
	for _, doc := range old {
		if _, err := db.ExecContext(ctx, `INSERT INTO session_events (session_id, doc) VALUES ('brave-otter', ?)`, doc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, `SELECT seq, doc FROM session_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]*pb.SessionEvent{}
	for rows.Next() {
		var seq int64
		var doc string
		if err := rows.Scan(&seq, &doc); err != nil {
			t.Fatal(err)
		}
		e := &pb.SessionEvent{}
		if err := protojson.Unmarshal([]byte(doc), e); err != nil {
			t.Fatal(err)
		}
		got[seq] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int64]*pb.SessionEvent{
		1: {At: 10, SessionId: "brave-otter", Event: &pb.SessionEvent_Command{Command: &pb.CommandUpdate{
			What: pb.CommandUpdate_WHAT_EXITED, CommandId: "c1", Argv: "go test", WorkspaceId: "w1",
			State: pb.CommandState_COMMAND_STATE_FAILED, ExitCode: proto.Int32(3),
		}}},
		2: {At: 11, SessionId: "brave-otter", Detail: "forked", Event: &pb.SessionEvent_Workspace{Workspace: &pb.WorkspaceUpdate{
			What: pb.WorkspaceUpdate_WHAT_READY, WorkspaceId: "w1",
		}}},
		3: {At: 12, SessionId: "brave-otter", Detail: "question 4 (Store) answered: SQLite", Event: &pb.SessionEvent_Question{Question: &pb.QuestionUpdate{
			What:     pb.QuestionUpdate_WHAT_ANSWERED,
			Question: &pb.Question{Id: 4, State: pb.QuestionState_QUESTION_STATE_ANSWERED, Answer: &pb.Answer{Picked: []string{"SQLite"}}},
		}}},
		4: {At: 13, SessionId: "brave-otter", Detail: "review 7 (X) failed", Event: &pb.SessionEvent_Review{Review: &pb.ReviewUpdate{
			What: pb.ReviewUpdate_WHAT_FAILED, ReviewId: 7,
		}}},
		6: {At: 15, SessionId: "brave-otter", Detail: "the user on review 7: ok", Event: &pb.SessionEvent_Review{Review: &pb.ReviewUpdate{
			What: pb.ReviewUpdate_WHAT_RESPONDED, ReviewId: 7,
		}}},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("session events after migration (-want +got):\n%s", diff)
	}
}
