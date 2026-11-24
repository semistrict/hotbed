package controller

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestSessionT3CodeThreadPersistenceAndSearch(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "hotbed.db")
	store, err := OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	for _, pname := range []string{"alpha", "beta"} {
		if _, err := store.AddProject(ctx, pname, nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(ServerConfig{Store: store, Now: clock})
	ss := &sessions{srv}

	// Reject oversized or control-char T3 Code thread IDs.
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "bad-len", Project: "alpha", T3CodeThreadId: strings.Repeat("x", maxT3CodeThreadIDLen+1),
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("oversized t3code_thread_id err = %v, want INVALID_ARGS", err)
	}
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "bad-nl", Project: "alpha", T3CodeThreadId: "thr-1\nthr-2",
	}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("newline t3code_thread_id err = %v, want INVALID_ARGS", err)
	}

	// Start an old ended session (7 days ago) in alpha for thr-1.
	now = now.Add(-7 * 24 * time.Hour)
	oldSess, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "old-fox", Project: "alpha", Agent: "laptop", T3CodeThreadId: "  thr-1  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if oldSess.T3CodeThreadId != "thr-1" {
		t.Fatalf("trimmed T3CodeThreadId = %q, want thr-1", oldSess.T3CodeThreadId)
	}
	now = now.Add(5 * time.Minute)
	if _, err := ss.EndSession(ctx, &pb.EndSessionRequest{Id: "old-fox"}); err != nil {
		t.Fatal(err)
	}

	// Start a more recently ended session (2 hours ago) in beta for thr-1.
	now = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "mid-owl", Project: "beta", Agent: "laptop", T3CodeThreadId: "thr-1",
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if _, err := ss.EndSession(ctx, &pb.EndSessionRequest{Id: "mid-owl"}); err != nil {
		t.Fatal(err)
	}

	// Start a live session (earlier last_active_at than an ended one would be,
	// to confirm live sessions sort ahead of ended sessions).
	now = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "live-elk", Project: "alpha", Agent: "laptop", T3CodeThreadId: "thr-1",
	}); err != nil {
		t.Fatal(err)
	}
	// Start a second live session for thr-1 that is more recently active.
	now = time.Date(2026, 11, 24, 11, 30, 0, 0, time.UTC)
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "live-top", Project: "beta", Agent: "laptop", T3CodeThreadId: "thr-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Start sessions with a prefix-overlapping thread ID and an empty thread ID.
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "other-fox", Project: "alpha", Agent: "laptop", T3CodeThreadId: "thr-10",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "plain-fox", Project: "alpha", Agent: "laptop",
	}); err != nil {
		t.Fatal(err)
	}

	// Verify persistence across DB reopen.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	srv = NewServer(ServerConfig{Store: store, Now: clock})
	ss = &sessions{srv}

	// SearchSessions: empty thread ID refused.
	if _, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("empty SearchSessions err = %v, want INVALID_ARGS", err)
	}
	// Unknown project refused.
	if _, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", Project: "nosuch"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("SearchSessions unknown project err = %v, want NOT_FOUND", err)
	}
	// Negative limit refused.
	neg := int32(-1)
	if _, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", Limit: &neg}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("SearchSessions negative limit err = %v, want INVALID_ARGS", err)
	}

	// SearchSessions across all projects and all ages: live first (by last_active_at DESC),
	// then ended (by last_active_at DESC), including 7-day-old old-fox; exact match excludes thr-10.
	res, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, len(res.Sessions))
	for i, s := range res.Sessions {
		gotIDs[i] = s.Id
		if s.T3CodeThreadId != "thr-1" {
			t.Errorf("session %s t3code_thread_id = %q, want thr-1", s.Id, s.T3CodeThreadId)
		}
	}
	wantIDs := []string{"live-top", "live-elk", "mid-owl", "old-fox"}
	if !slices.Equal(gotIDs, wantIDs) || res.More != 0 {
		t.Fatalf("SearchSessions(thr-1) = %v (more=%d), want %v (more=0)", gotIDs, res.More, wantIDs)
	}

	// SearchSessions does not touch last_active_at.
	beforeTouch := res.Sessions[0].LastActiveAt
	now = now.Add(10 * time.Minute)
	res2, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Sessions[0].LastActiveAt != beforeTouch {
		t.Fatalf("SearchSessions reset last_active_at: %d -> %d", beforeTouch, res2.Sessions[0].LastActiveAt)
	}

	// live_only=true returns only live sessions.
	liveOnly, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", LiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(liveOnly.Sessions); !slices.Equal(got, []string{"live-top", "live-elk"}) {
		t.Fatalf("SearchSessions(live_only) = %v, want [live-top live-elk]", got)
	}

	// project filter narrows to one project.
	alphaOnly, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(alphaOnly.Sessions); !slices.Equal(got, []string{"live-elk", "old-fox"}) {
		t.Fatalf("SearchSessions(project=alpha) = %v, want [live-elk old-fox]", got)
	}

	// limit=2 returns top 2 and reports more=2; limit=0 returns all 4.
	lim2 := int32(2)
	paged, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", Limit: &lim2})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(paged.Sessions); !slices.Equal(got, []string{"live-top", "live-elk"}) || paged.More != 2 {
		t.Fatalf("SearchSessions(limit=2) = %v (more=%d), want [live-top live-elk] (more=2)", got, paged.More)
	}
	lim0 := int32(0)
	unlim, err := ss.SearchSessions(ctx, &pb.SearchSessionsRequest{T3CodeThreadId: "thr-1", Limit: &lim0})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(unlim.Sessions); !slices.Equal(got, wantIDs) || unlim.More != 0 {
		t.Fatalf("SearchSessions(limit=0) = %v (more=%d), want %v (more=0)", got, unlim.More, wantIDs)
	}

	// GetSession by t3code_thread_id returns the highest-priority match (live-top).
	gotByThread, err := ss.GetSession(ctx, &pb.GetSessionRequest{T3CodeThreadId: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotByThread.Session.Id != "live-top" || gotByThread.Session.T3CodeThreadId != "thr-1" {
		t.Fatalf("GetSession(thr-1) = %v, want live-top", gotByThread.Session)
	}
	if _, err := ss.GetSession(ctx, &pb.GetSessionRequest{T3CodeThreadId: "no-such-thread"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("GetSession(no-such-thread) err = %v, want NOT_FOUND", err)
	}
	if _, err := ss.GetSession(ctx, &pb.GetSessionRequest{Id: "live-top", T3CodeThreadId: "thr-1"}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("GetSession(both) err = %v, want INVALID_ARGS", err)
	}
	if _, err := ss.GetSession(ctx, &pb.GetSessionRequest{}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("GetSession(neither) err = %v, want INVALID_ARGS", err)
	}

	// ListSessions with t3code_thread_id works without scope and respects ended window (24h).
	lsLive, err := ss.ListSessions(ctx, &pb.ListSessionsRequest{T3CodeThreadId: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(lsLive.Sessions); !slices.Equal(got, []string{"live-top", "live-elk"}) {
		t.Fatalf("ListSessions(thr-1) = %v, want [live-top live-elk]", got)
	}
	lsEnded, err := ss.ListSessions(ctx, &pb.ListSessionsRequest{T3CodeThreadId: "thr-1", Ended: true})
	if err != nil {
		t.Fatal(err)
	}
	// mid-owl ended 2h ago (within 24h), old-fox ended 7d ago (outside 24h).
	if got := idsOf(lsEnded.Sessions); !slices.Equal(got, []string{"live-top", "mid-owl", "live-elk"}) {
		t.Fatalf("ListSessions(thr-1, ended=true) = %v, want [live-top mid-owl live-elk]", got)
	}
	lsAlpha, err := ss.ListSessions(ctx, &pb.ListSessionsRequest{
		T3CodeThreadId: "thr-1", Scope: &pb.ListSessionsRequest_Project{Project: "alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(lsAlpha.Sessions); !slices.Equal(got, []string{"live-elk"}) {
		t.Fatalf("ListSessions(thr-1, project=alpha) = %v, want [live-elk]", got)
	}

	// Resume semantics:
	// 1. Resuming a session with empty t3code_thread_id associates the new one.
	if _, err := ss.EndSession(ctx, &pb.EndSessionRequest{Id: "plain-fox"}); err != nil {
		t.Fatal(err)
	}
	resumedPlain, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "plain-fox", Resume: true, T3CodeThreadId: "thr-backfill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumedPlain.T3CodeThreadId != "thr-backfill" {
		t.Fatalf("resumed plain-fox t3code_thread_id = %q, want thr-backfill", resumedPlain.T3CodeThreadId)
	}
	// 2. Resuming a session that already has a t3code_thread_id keeps its original one
	// whether resumed with another ID or with none.
	resumedKeep1, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "plain-fox", Resume: true, T3CodeThreadId: "thr-other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumedKeep1.T3CodeThreadId != "thr-backfill" {
		t.Fatalf("resumed with different thread overwrote it: got %q, want thr-backfill", resumedKeep1.T3CodeThreadId)
	}
	resumedKeep2, err := ss.StartSession(ctx, &pb.StartSessionRequest{
		Id: "plain-fox", Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumedKeep2.T3CodeThreadId != "thr-backfill" {
		t.Fatalf("resumed with empty thread cleared it: got %q, want thr-backfill", resumedKeep2.T3CodeThreadId)
	}

	// Verify SQLite uses the partial index sessions_t3code_thread_id for searches.
	rows, err := store.db.QueryContext(ctx, `EXPLAIN QUERY PLAN
		SELECT s.id, s.agent, p.name AS project, s.t3code_thread_id, s.started_at, s.last_active_at, s.ended_at, s.end_reason
		FROM sessions s
		JOIN projects p ON p.id = s.project_id
		WHERE s.t3code_thread_id = CAST(?1 AS TEXT)
		  AND s.t3code_thread_id != ''
		  AND (CAST(?2 AS INTEGER) = 0 OR s.ended_at IS NULL)
		  AND p.name = COALESCE(NULLIF(CAST(?3 AS TEXT), ''), p.name)
		ORDER BY (s.ended_at IS NOT NULL) ASC, s.last_active_at DESC, s.started_at DESC, s.id ASC
		LIMIT CAST(?4 AS INTEGER)`, "thr-1", 0, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if !strings.Contains(plan.String(), "sessions_t3code_thread_id") {
		t.Errorf("SearchSessionsByT3CodeThread query plan does not use sessions_t3code_thread_id index:\n%s", plan.String())
	}
}

func idsOf(ss []*pb.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Id
	}
	return out
}
