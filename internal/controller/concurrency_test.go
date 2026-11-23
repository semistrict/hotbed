package controller_test

import (
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestOptimisticConcurrency(t *testing.T) {
	store, err := controller.OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tn := tailnettest.New(ubu1)
	srv := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store:      store,
		Identifier: tn,
		Cluster:    statedir.ControllerConfig{ClusterID: "c0ffee", Owner: "ramon@", Port: 7120},
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
	}).Handler())
	t.Cleanup(srv.Close)

	hc := tn.Client(tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"})
	conn := connect.NewClient(connecthttp.NewTransport(hc, srv.URL))
	cluster := hotbedv1connect.NewClusterServiceClient(conn)
	sessions := hotbedv1connect.NewSessionServiceClient(conn)
	questions := hotbedv1connect.NewQuestionServiceClient(conn)
	reviews := hotbedv1connect.NewReviewServiceClient(conn)

	ctx := t.Context()
	if _, err := cluster.AddProject(ctx, &pb.AddProjectRequest{Name: "demo", Repos: []string{"github.com/acme/demo"}}); err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.StartSession(ctx, &pb.StartSessionRequest{Id: "alpha-fox", Project: "demo"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("question if-revision", func(t *testing.T) {
		prompt := &pb.Prompt{
			Question: "Which store?",
			Header:   "Store",
			Options: []*pb.PromptOption{
				{Label: "SQLite", Description: "Simple"},
				{Label: "Postgres", Description: "Server"},
			},
		}
		added, err := questions.AddQuestions(ctx, &pb.AddQuestionsRequest{SessionId: sess.Id, Questions: []*pb.Prompt{prompt}})
		if err != nil {
			t.Fatal(err)
		}
		q := added.Questions[0]
		if q.Revision != 1 {
			t.Fatalf("initial revision = %d, want 1", q.Revision)
		}
		// Matching revision succeeds and bumps revision to 2.
		edited, err := questions.EditQuestion(ctx, &pb.EditQuestionRequest{
			Id: q.Id, SessionId: sess.Id, IfRevision: 1,
			Prompt: &pb.Prompt{
				Question: "Which store now?",
				Header:   "Store",
				Options:  prompt.Options,
			},
		})
		if err != nil {
			t.Fatalf("EditQuestion with matching revision: %v", err)
		}
		if edited.Revision != 2 {
			t.Fatalf("after edit revision = %d, want 2", edited.Revision)
		}
		// Stale revision fails with CONFLICT on edit, answer, dismiss, delete.
		if _, err := questions.EditQuestion(ctx, &pb.EditQuestionRequest{
			Id: q.Id, SessionId: sess.Id, IfRevision: 1, Prompt: prompt,
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale EditQuestion = %v, want CONFLICT", err)
		}
		if _, err := questions.AnswerQuestion(ctx, &pb.AnswerQuestionRequest{
			Id: q.Id, IfRevision: 1, Answer: &pb.Answer{Picked: []string{"SQLite"}},
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale AnswerQuestion = %v, want CONFLICT", err)
		}
		if _, err := questions.DismissQuestion(ctx, &pb.DismissQuestionRequest{
			Id: q.Id, IfRevision: 1, Reason: "no",
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale DismissQuestion = %v, want CONFLICT", err)
		}
		if _, err := questions.DeleteQuestion(ctx, &pb.DeleteQuestionRequest{
			Id: q.Id, SessionId: sess.Id, IfRevision: 1, Reason: "no",
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale DeleteQuestion = %v, want CONFLICT", err)
		}
		// Current revision succeeds on answer.
		answered, err := questions.AnswerQuestion(ctx, &pb.AnswerQuestionRequest{
			Id: q.Id, IfRevision: 2, Answer: &pb.Answer{Picked: []string{"SQLite"}},
		})
		if err != nil {
			t.Fatalf("AnswerQuestion with matching revision: %v", err)
		}
		if answered.Revision != 3 {
			t.Fatalf("after answer revision = %d, want 3", answered.Revision)
		}
	})

	t.Run("step if-revision", func(t *testing.T) {
		p1, err := cluster.PutStep(ctx, &pb.PutStepRequest{
			Project: "demo", Step: &pb.Step{Name: "deps", Script: "echo 1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if p1.SetupRevision != 1 {
			t.Fatalf("setup revision = %d, want 1", p1.SetupRevision)
		}
		// Matching revision succeeds and bumps revision to 2.
		p2, err := cluster.PutStep(ctx, &pb.PutStepRequest{
			Project: "demo", IfRevision: 1, Step: &pb.Step{Name: "deps", Script: "echo 2"},
		})
		if err != nil {
			t.Fatalf("PutStep with matching revision: %v", err)
		}
		if p2.SetupRevision != 2 {
			t.Fatalf("setup revision = %d, want 2", p2.SetupRevision)
		}
		// Stale revision fails with CONFLICT on PutStep and DeleteStep.
		if _, err := cluster.PutStep(ctx, &pb.PutStepRequest{
			Project: "demo", IfRevision: 1, Step: &pb.Step{Name: "deps", Script: "echo 3"},
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale PutStep = %v, want CONFLICT", err)
		}
		if _, err := cluster.DeleteStep(ctx, &pb.DeleteStepRequest{
			Project: "demo", Name: "deps", IfRevision: 1,
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale DeleteStep = %v, want CONFLICT", err)
		}
		// Current revision succeeds on DeleteStep.
		p3, err := cluster.DeleteStep(ctx, &pb.DeleteStepRequest{
			Project: "demo", Name: "deps", IfRevision: 2,
		})
		if err != nil {
			t.Fatalf("DeleteStep with matching revision: %v", err)
		}
		if p3.SetupRevision != 3 {
			t.Fatalf("after delete setup revision = %d, want 3", p3.SetupRevision)
		}
	})

	t.Run("review respond after", func(t *testing.T) {
		rev, err := reviews.RequestReview(ctx, &pb.RequestReviewRequest{
			SessionId: sess.Id, Title: "Check concurrency", Head: "abcdef1234567",
		})
		if err != nil {
			t.Fatal(err)
		}
		e1, err := reviews.Respond(ctx, &pb.RespondRequest{ReviewId: rev.Id, Text: "first"})
		if err != nil {
			t.Fatal(err)
		}
		// Matching after (latest seq) succeeds.
		e2, err := reviews.Respond(ctx, &pb.RespondRequest{ReviewId: rev.Id, Text: "second", After: e1.Seq})
		if err != nil {
			t.Fatalf("Respond with matching after: %v", err)
		}
		if e2.Seq <= e1.Seq {
			t.Fatalf("e2.Seq = %d, want > %d", e2.Seq, e1.Seq)
		}
		// Stale after (e1.Seq now behind e2.Seq) fails with CONFLICT.
		if _, err := reviews.Respond(ctx, &pb.RespondRequest{
			ReviewId: rev.Id, Text: "stale", After: e1.Seq,
		}); api.CodeOf(err) != api.CodeConflict {
			t.Errorf("stale Respond = %v, want CONFLICT", err)
		}
	})
}
