package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestSessionT3CodeThreadPersistenceAndSearchCLI(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "project", "add", "other")

	// Start a session with T3CODE_THREAD_ID set: persisted on the server.
	t.Setenv("T3CODE_THREAD_ID", "t3-thread-alpha")
	var first pb.Session
	c.ok(&first, "session", "start", "alpha-one")
	if first.T3CodeThreadId != "t3-thread-alpha" {
		t.Fatalf("session start T3CodeThreadId = %q, want t3-thread-alpha", first.T3CodeThreadId)
	}

	// Plain text output of session start includes t3=...
	c.ok(nil, "session", "end")
	code, plainStart, stderr := c.hotbed("session", "start", "alpha-two")
	if code != exitOK || !strings.Contains(plainStart, "t3=t3-thread-alpha") {
		t.Fatalf("plain session start: code=%d stdout=%q stderr=%q, want t3=t3-thread-alpha", code, plainStart, stderr)
	}

	// Run a command so alpha-two has a workspace and command, then advance clock >24h after ending alpha-one.
	if code, _, stderr := c.hotbed("run", "--", "sh", "-c", "echo ok"); code != 0 {
		t.Fatalf("run: exit %d, stderr %q", code, stderr)
	}
	c.eventually("command in alpha-two", func() bool {
		var st sessionStatusResult
		c.ok(&st, "session", "status")
		return len(st.Commands) == 1 && st.T3CodeThreadID == "t3-thread-alpha"
	})

	// Start another session in project "other" with explicit --t3code-thread-id override.
	var otherSess pb.Session
	c.ok(&otherSess, "session", "start", "beta-one", "-p", "other", "--t3code-thread-id", "t3-thread-alpha", "--replace")
	if otherSess.T3CodeThreadId != "t3-thread-alpha" {
		t.Fatalf("session start --t3code-thread-id = %q, want t3-thread-alpha", otherSess.T3CodeThreadId)
	}

	// Start a session for a different thread ID to verify exact matching.
	t.Setenv("T3CODE_THREAD_ID", "t3-thread-alpha-2")
	c.ok(nil, "session", "start", "gamma-one", "-p", "other")

	// Advance clock by 48h so ended sessions (alpha-one, alpha-two) are outside ListSessions' 24h window.
	c.clock.advance(48 * time.Hour)
	// Restart controller to confirm server-side SQLite persistence across restarts.
	c.restart()

	// Unset T3CODE_THREAD_ID and bind nothing locally (simulating another machine / shell).
	t.Setenv("T3CODE_THREAD_ID", "")
	t.Setenv("HOTBED_SESSION", "")
	t.Setenv("HOTBED_PROJECT", "other") // must NOT restrict `session search` unless -p is passed

	// 1. `session search THREAD_ID` finds all 3 sessions across projects (live first, then ended even >24h old).
	var found pb.SearchSessionsResponse
	c.ok(&found, "session", "search", "t3-thread-alpha")
	gotIDs := make([]string, len(found.Sessions))
	for i, s := range found.Sessions {
		gotIDs[i] = s.Id
		if s.CockpitUrl == "" {
			t.Errorf("session search %s missing cockpitUrl", s.Id)
		}
	}
	if !slices.Equal(gotIDs, []string{"beta-one", "alpha-two", "alpha-one"}) || found.More != 0 {
		t.Fatalf("session search t3-thread-alpha = %v (more=%d), want [beta-one alpha-two alpha-one] (more=0)", gotIDs, found.More)
	}

	// 2. `session search --t3code-thread-id` flag form, `--live`, `-p`, and `-n` pagination.
	var liveOnly pb.SearchSessionsResponse
	c.ok(&liveOnly, "session", "search", "--t3code-thread-id", "t3-thread-alpha", "--live")
	if len(liveOnly.Sessions) != 1 || liveOnly.Sessions[0].Id != "beta-one" {
		t.Fatalf("session search --live = %v, want [beta-one]", liveOnly.Sessions)
	}

	var projOnly pb.SearchSessionsResponse
	c.ok(&projOnly, "session", "search", "t3-thread-alpha", "-p", "app")
	if len(projOnly.Sessions) != 2 || projOnly.Sessions[0].Id != "alpha-two" || projOnly.Sessions[1].Id != "alpha-one" {
		t.Fatalf("session search -p app = %v, want [alpha-two alpha-one]", projOnly.Sessions)
	}

	var limited pb.SearchSessionsResponse
	c.ok(&limited, "session", "search", "t3-thread-alpha", "-n", "1")
	if len(limited.Sessions) != 1 || limited.Sessions[0].Id != "beta-one" || limited.More != 2 {
		t.Fatalf("session search -n 1 = %v (more=%d), want [beta-one] (more=2)", limited.Sessions, limited.More)
	}

	// Plain text output of `session search` shows t3=... and the (+N older matching...) notice.
	code, plainSearch, stderr := c.hotbed("session", "search", "t3-thread-alpha", "-n", "1")
	if code != exitOK || !strings.Contains(plainSearch, "beta-one") || !strings.Contains(plainSearch, "t3=t3-thread-alpha") || !strings.Contains(plainSearch, "(+2 older matching") {
		t.Fatalf("plain session search: code=%d stdout=%q stderr=%q", code, plainSearch, stderr)
	}

	// 3. `session search` with no args defaults to T3CODE_THREAD_ID; explicit arg overrides it.
	if code, e := c.fails("session", "search"); code != exitError || e.Code != api.CodeInvalidArgs {
		t.Fatalf("session search with no id or env: exit %d, %+v, want INVALID_ARGS", code, e)
	}
	if code, e := c.fails("session", "search", "a", "--t3code-thread-id", "b"); code != exitError || e.Code != api.CodeInvalidArgs {
		t.Fatalf("session search with both arg and flag: exit %d, %+v, want INVALID_ARGS", code, e)
	}
	t.Setenv("T3CODE_THREAD_ID", "t3-thread-alpha-2")
	var envSearch pb.SearchSessionsResponse
	c.ok(&envSearch, "session", "search")
	if len(envSearch.Sessions) != 1 || envSearch.Sessions[0].Id != "gamma-one" {
		t.Fatalf("session search from T3CODE_THREAD_ID = %v, want [gamma-one]", envSearch.Sessions)
	}
	var overrideSearch pb.SearchSessionsResponse
	c.ok(&overrideSearch, "session", "search", "t3-thread-alpha")
	if len(overrideSearch.Sessions) != 3 {
		t.Fatalf("session search explicit arg with T3CODE_THREAD_ID set = %d, want 3", len(overrideSearch.Sessions))
	}

	// 4. `session status --t3code-thread-id` looks up directly on controller (works without local sessions.json).
	t.Setenv("T3CODE_THREAD_ID", "")
	var stByThread sessionStatusResult
	c.ok(&stByThread, "session", "status", "--t3code-thread-id", "t3-thread-alpha")
	if !stByThread.Active || stByThread.ID != "beta-one" || stByThread.Source != "t3code-thread" || stByThread.Key != "t3-thread-alpha" || stByThread.T3CodeThreadID != "t3-thread-alpha" {
		t.Fatalf("session status --t3code-thread-id = %+v, want active beta-one", stByThread)
	}
	if _, e := c.fails("session", "status", "--t3code-thread-id", "no-such-t3"); e.Code != api.CodeNotFound {
		t.Fatalf("session status --t3code-thread-id unknown = %+v, want NOT_FOUND", e)
	}
	if _, e := c.fails("session", "status", "-s", "beta-one", "--t3code-thread-id", "t3-thread-alpha"); e.Code != api.CodeInvalidArgs {
		t.Fatalf("session status with -s and --t3code-thread-id = %+v, want INVALID_ARGS", e)
	}

	// 5. `session ls --t3code-thread-id` filters ListSessions.
	t.Setenv("HOTBED_PROJECT", "")
	var lsFiltered pb.ListSessionsResponse
	c.ok(&lsFiltered, "session", "ls", "--t3code-thread-id", "t3-thread-alpha")
	if len(lsFiltered.Sessions) != 1 || lsFiltered.Sessions[0].Id != "beta-one" {
		t.Fatalf("session ls --t3code-thread-id = %v, want [beta-one]", lsFiltered.Sessions)
	}

	// 6. `hotbed mcp` inside T3 Code also persists T3CODE_THREAD_ID server-side.
	t.Setenv("T3CODE_THREAD_ID", "t3-thread-mcp-persist")
	t.Setenv("HOTBED_PROJECT", "app")
	m := c.mcp()
	defer m.stop()
	var mcpFound pb.SearchSessionsResponse
	c.ok(&mcpFound, "session", "search", "t3-thread-mcp-persist")
	if len(mcpFound.Sessions) != 1 || mcpFound.Sessions[0].T3CodeThreadId != "t3-thread-mcp-persist" {
		t.Fatalf("MCP session not persisted with T3CODE_THREAD_ID: %v", mcpFound.Sessions)
	}
}
