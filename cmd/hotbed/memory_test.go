package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestMemoryLifecycleAndSessionRecall(t *testing.T) {
	c := newCluster(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "brave-otter")
	c.ok(nil, "session", "start", "calm-heron")
	c.ok(nil, "session", "start", "other-fox", "-p", "other")

	// Saving a global always-on rule from an agent session is automatically approved immediately.
	mem1 := &pb.Memory{}
	c.ok(mem1, "memory", "propose", "-s", "brave-otter",
		"--global", "--mode", "always",
		"--slug", "subagent-model-neon",
		"--title", "Subagent model",
		"--summary", "Use gemini-4-neon (not GPT models) for all T3 Code delegated subagents.",
		"--rationale", "User correction in thread fxdax",
		"--model", "gemini-4-neon",
		"--tag", "subagents", "--tag", "models",
	)
	if mem1.Id != 1 || mem1.State != pb.MemoryState_MEMORY_STATE_APPROVED || mem1.Scope != pb.MemoryScope_MEMORY_SCOPE_GLOBAL {
		t.Fatalf("mem1 = %v, want APPROVED GLOBAL", mem1)
	}
	if mem1.CockpitUrl == "" || !strings.HasSuffix(mem1.CockpitUrl, "/memories/1") {
		t.Errorf("mem1 cockpitUrl = %q, want suffix /memories/1", mem1.CockpitUrl)
	}
	if mem1.GetReminder() != "" {
		t.Errorf("auto-approved memory should carry no approval poll reminder, got %q", mem1.GetReminder())
	}

	// Saving a project-scoped on-demand memory with --body-file - (stdin) is also auto-approved.
	mem2 := &pb.Memory{}
	c.okIn("# Debugging ZFS flakes\n\nRun `zpool status` first.\n", mem2,
		"memory", "propose", "-s", "brave-otter",
		"--slug", "zfs-base-freshen",
		"--title", "ZFS base freshening",
		"--summary", "How to debug ZFS snapshot/clone base freshening flakes.",
		"--rationale", "Debugging session",
		"--tag", "zfs", "--tag", "bases",
		"--body-file", "-",
	)
	if mem2.Id != 2 || mem2.State != pb.MemoryState_MEMORY_STATE_APPROVED || mem2.Scope != pb.MemoryScope_MEMORY_SCOPE_PROJECT || mem2.Project != "test" {
		t.Fatalf("mem2 = %v, want APPROVED PROJECT test", mem2)
	}

	// Default `memory ls` immediately shows both active memories (no approval step).
	activeList := &pb.ListMemoriesResponse{}
	c.ok(activeList, "memory", "ls", "-s", "brave-otter")
	if len(activeList.Memories) != 2 {
		t.Fatalf("active ls = %d, want 2", len(activeList.Memories))
	}

	// Snapshot reports 0 pending memories.
	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	if snap.PendingMemories != 0 || snap.GlobalPendingMemories != 0 {
		t.Errorf("snapshot pendingMemories=%d global=%d, want 0 and 0", snap.PendingMemories, snap.GlobalPendingMemories)
	}

	// Starting a new session immediately returns both memories in compact form (plain text + JSON).
	started := &pb.Session{}
	c.ok(started, "session", "start", "keen-fox")
	if started.Memories == nil || len(started.Memories.Always) != 1 || len(started.Memories.OnDemand) != 1 {
		t.Fatalf("session start memories = %+v", started.Memories)
	}
	if started.Memories.Always[0].Slug != "subagent-model-neon" || started.Memories.OnDemand[0].Slug != "zfs-base-freshen" {
		t.Errorf("unexpected briefing slugs: always=%v onDemand=%v", started.Memories.Always, started.Memories.OnDemand)
	}

	// Plain-text session start renders both sections cleanly.
	var plainOutBuf, plainErrBuf bytes.Buffer
	plainDeps := c.cli
	plainDeps.stdout, plainDeps.stderr = &plainOutBuf, &plainErrBuf
	code := run(t.Context(), []string{"session", "start", "wise-owl"}, plainDeps)
	if code != exitOK {
		t.Fatalf("plain session start failed (%d): %s", code, plainErrBuf.String())
	}
	plainOut := plainOutBuf.String()
	for _, want := range []string{"Memories (1 always-on, 1 on-demand", "Always-on:", "[global] subagent-model-neon:", "On-demand (read when relevant):", "[test] zfs-base-freshen —"} {
		if !strings.Contains(plainOut, want) {
			t.Errorf("plain session start output missing %q:\n%s", want, plainOut)
		}
	}

	// --omit-memories suppresses the briefing.
	bare := &pb.Session{}
	c.ok(bare, "session", "start", "quiet-elk", "--omit-memories")
	if bare.Memories != nil {
		t.Errorf("expected nil memories with --omit-memories, got %+v", bare.Memories)
	}

	// On-demand full retrieval via `hotbed memory get` fetches multiple refs with full body.
	got := &pb.GetMemoriesResponse{}
	c.ok(got, "memory", "get", "-s", "keen-fox", "global/subagent-model-neon", "zfs-base-freshen")
	if len(got.Memories) != 2 {
		t.Fatalf("memory get returned %d, want 2", len(got.Memories))
	}
	if !strings.Contains(got.Memories[1].Content.Body, "zpool status") {
		t.Errorf("memory get body = %q, want zpool status", got.Memories[1].Content.Body)
	}

	// `memory ls` omits full body (progressive disclosure) and supports -q search.
	searched := &pb.ListMemoriesResponse{}
	c.ok(searched, "memory", "ls", "-s", "keen-fox", "-q", "zpool")
	if len(searched.Memories) != 1 || searched.Memories[0].Content.Slug != "zfs-base-freshen" || searched.Memories[0].Content.Body != "" {
		t.Fatalf("memory ls -q zpool = %+v", searched.Memories)
	}

	// Edit a memory in any way: partial edits, clearing title/tags/rationale/body, changing mode/slug.
	edited1 := &pb.Memory{}
	c.ok(edited1, "memory", "edit", "1",
		"--summary", "Use gemini-4-neon (high) for subagents.",
		"--clear-title",
		"--replace-tags",
		"--rationale", "",
		"--if-revision", fmt.Sprint(mem1.Revision),
	)
	if edited1.Content.Title != "" || len(edited1.Content.Tags) != 0 || edited1.Provenance.GetRationale() != "" {
		t.Errorf("expected cleared title/tags/rationale, got title=%q tags=%v rationale=%q",
			edited1.Content.Title, edited1.Content.Tags, edited1.Provenance.GetRationale())
	}
	if edited1.Content.Summary != "Use gemini-4-neon (high) for subagents." {
		t.Errorf("edited1 summary = %q", edited1.Content.Summary)
	}

	// Full content replacement (--replace-content) lets users edit or clear any content field in one shot.
	replaced1 := &pb.Memory{}
	c.ok(replaced1, "memory", "edit", "1",
		"--replace-content",
		"--slug", "subagent-model-neon",
		"--mode", "always",
		"--summary", "Use gemini-4-neon for all T3 Code subagents.",
		"--tag", "agents",
	)
	if replaced1.Content.Title != "" || replaced1.Content.Body != "" || !slices.Equal(replaced1.Content.Tags, []string{"agents"}) {
		t.Errorf("replaced1 content = %+v", replaced1.Content)
	}

	// Promote project-specific memory #2 to global via `hotbed memory promote`.
	// Before promotion, project "other" does NOT see #2.
	otherBefore := &pb.ListMemoriesResponse{}
	c.ok(otherBefore, "memory", "ls", "-p", "other")
	if len(otherBefore.Memories) != 1 || otherBefore.Memories[0].Id != 1 {
		t.Fatalf("other project before promote = %+v, want only #1", otherBefore.Memories)
	}

	promoted2 := &pb.Memory{}
	c.ok(promoted2, "memory", "promote", "2")
	if promoted2.Scope != pb.MemoryScope_MEMORY_SCOPE_GLOBAL || promoted2.Project != "" || promoted2.Repo != "" {
		t.Fatalf("promoted2 = scope=%v project=%q repo=%q, want GLOBAL empty", promoted2.Scope, promoted2.Project, promoted2.Repo)
	}
	// After promotion, project "other" sees #2 too!
	otherAfter := &pb.ListMemoriesResponse{}
	c.ok(otherAfter, "memory", "ls", "-p", "other")
	if len(otherAfter.Memories) != 2 {
		t.Fatalf("other project after promote = %d, want 2", len(otherAfter.Memories))
	}
	// ShowMemory records the "promoted" history entry.
	shown2 := &pb.Memory{}
	c.ok(shown2, "memory", "show", "2")
	if !slices.ContainsFunc(shown2.History, func(ch *pb.MemoryChange) bool { return ch.What == "promoted" }) {
		t.Errorf("expected 'promoted' entry in memory #2 history, got %+v", shown2.History)
	}

	// Demote #2 back to project "test" via `memory edit -p test` to verify scope editing in both directions.
	demoted2 := &pb.Memory{}
	c.ok(demoted2, "memory", "edit", "2", "-p", "test")
	if demoted2.Scope != pb.MemoryScope_MEMORY_SCOPE_PROJECT || demoted2.Project != "test" {
		t.Fatalf("demoted2 = scope=%v project=%q, want PROJECT test", demoted2.Scope, demoted2.Project)
	}

	// Superseding an existing memory immediately transitions old -> superseded and keeps slug working.
	mem3 := &pb.Memory{}
	c.ok(mem3, "memory", "propose", "-s", "brave-otter",
		"--global", "--mode", "always",
		"--slug", "subagent-model-neon",
		"--supersede", "1",
		"--summary", "Use gemini-4-neon (high) for all T3 Code subagents; never GPT.",
	)
	if mem3.State != pb.MemoryState_MEMORY_STATE_APPROVED {
		t.Fatalf("mem3 state = %v, want APPROVED", mem3.State)
	}
	old1 := &pb.Memory{}
	c.ok(old1, "memory", "show", "1")
	if old1.State != pb.MemoryState_MEMORY_STATE_SUPERSEDED || old1.SupersededBy != mem3.Id {
		t.Fatalf("old1 after supersede = state=%v supersededBy=%d", old1.State, old1.SupersededBy)
	}
	gotNew := &pb.GetMemoriesResponse{}
	c.ok(gotNew, "memory", "get", "subagent-model-neon")
	if len(gotNew.Memories) != 1 || gotNew.Memories[0].Id != mem3.Id {
		t.Fatalf("slug lookup after supersede = %+v, want #%d", gotNew.Memories, mem3.Id)
	}

	// Move, archive, restore, and permanently remove (`memory rm`).
	moved := &pb.Memory{}
	c.ok(moved, "memory", "move", "2", "--first")
	if moved.Id != 2 {
		t.Errorf("moved = %v", moved)
	}

	arch := &pb.Memory{}
	c.ok(arch, "memory", "archive", "2", "--reason", "no longer needed")
	if arch.State != pb.MemoryState_MEMORY_STATE_ARCHIVED {
		t.Fatalf("archived state = %v", arch.State)
	}

	rest := &pb.Memory{}
	c.ok(rest, "memory", "restore", "2")
	if rest.State != pb.MemoryState_MEMORY_STATE_APPROVED {
		t.Fatalf("restored state = %v", rest.State)
	}

	// Permanently remove #2 via `hotbed memory rm`.
	if _, e := c.fails("memory", "rm", "2", "--if-revision", "999"); e.Code != api.CodeConflict {
		t.Fatalf("rm with wrong revision = %v, want CONFLICT", e)
	}
	removed := &pb.Memory{}
	c.ok(removed, "memory", "rm", "2", "--if-revision", fmt.Sprint(rest.Revision))
	if removed.Id != 2 {
		t.Fatalf("removed = %+v, want id 2", removed)
	}
	if _, e := c.fails("memory", "show", "2"); e.Code != api.CodeNotFound {
		t.Fatalf("show after rm = %v, want NOT_FOUND", e)
	}
	// Slug of removed memory can be reused cleanly!
	reused := &pb.Memory{}
	c.ok(reused, "memory", "propose", "-p", "test", "--mode", "always",
		"--slug", "zfs-base-freshen", "--summary", "Fresh memory reusing removed slug.")
	if reused.State != pb.MemoryState_MEMORY_STATE_APPROVED || reused.Content.Slug != "zfs-base-freshen" {
		t.Fatalf("reused = %+v", reused)
	}
}

func TestMCPServerIncludesMemoriesInInstructionsAndTools(t *testing.T) {
	c := newCluster(t)
	// Seed a global memory directly (user action, no session bound, auto-approved).
	c.ok(nil, "memory", "propose", "--global", "--mode", "always",
		"--slug", "mcp-test-rule", "--summary", "Always prefer neon over GPT.")

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("T3CODE_THREAD_ID", "thread-mcp-mem")
	m := c.mcp()
	defer m.stop()

	instr := m.InitializeResult().Instructions
	if !strings.Contains(instr, "mcp-test-rule") || !strings.Contains(instr, "Always prefer neon over GPT.") {
		t.Errorf("MCP server instructions missing active memory:\n%s", instr)
	}
	if !strings.Contains(instr, "memory_propose") || !strings.Contains(instr, "hotbed memory propose") {
		t.Errorf("MCP server instructions missing memory-saving reminder:\n%s", instr)
	}

	// Fetch via memory_get MCP tool.
	got := &pb.GetMemoriesResponse{}
	m.call(got, "memory_get", map[string]any{"ref": []string{"mcp-test-rule"}})
	if len(got.Memories) != 1 || got.Memories[0].Content.Slug != "mcp-test-rule" {
		t.Fatalf("MCP memory_get = %+v", got)
	}

	// Save via memory_propose MCP tool (inherits MCP session automatically and auto-approves).
	prop := &pb.Memory{}
	m.call(prop, "memory_propose", map[string]any{
		"global":  true,
		"mode":    "on_demand",
		"slug":    "mcp-playbook",
		"summary": "Playbook saved over MCP",
		"body":    "Step 1: verify MCP.\n",
	})
	if prop.State != pb.MemoryState_MEMORY_STATE_APPROVED || prop.Proposed.Session == "" {
		t.Fatalf("MCP memory_propose = %+v, want APPROVED with session", prop)
	}
	_ = filepath.Clean(dir)
}

func TestMemorySavingReminders(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))

	// 1. Session start (plain + JSON + resume) includes the memory-saving reminder even with 0 memories.
	code, stdout, stderr := c.hotbed("session", "start", "bright-jay")
	if code != 0 {
		t.Fatalf("session start failed (%d): %s", code, stderr)
	}
	for _, want := range []string{"hotbed memory propose", "something goes wrong", "memory_propose"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plain session start missing memory reminder %q:\n%s", want, stdout)
		}
	}

	sess := &pb.Session{}
	c.ok(sess, "session", "start", "swift-fox")
	if !strings.Contains(sess.GetReminder(), "hotbed memory propose") {
		t.Errorf("json session start reminder = %q, want memory propose prompt", sess.GetReminder())
	}
	c.ok(nil, "session", "end", "-s", "swift-fox")
	resumed := &pb.Session{}
	c.ok(resumed, "session", "start", "swift-fox", "--resume")
	if !strings.Contains(resumed.GetReminder(), "hotbed memory propose") {
		t.Errorf("resumed session reminder = %q, want memory propose prompt", resumed.GetReminder())
	}

	// 2. Attached `hotbed run` prints a memory reminder on stderr on non-zero exit, not on exit 0.
	c.workspace("bright-jay", nil)
	code, _, stderr = c.hotbed("run", "-s", "bright-jay", "-r", "app", "--", "true")
	if code != 0 {
		t.Fatalf("run true failed (%d): %s", code, stderr)
	}
	if strings.Contains(stderr, "hotbed memory propose") {
		t.Errorf("exit 0 run unexpectedly printed memory reminder:\n%s", stderr)
	}

	code, _, stderr = c.hotbed("run", "-s", "bright-jay", "-r", "app", "--", "sh", "-c", "exit 7")
	if code != 7 {
		t.Fatalf("run exit 7 returned %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "hotbed memory propose") {
		t.Errorf("failed run stderr missing memory reminder:\n%s", stderr)
	}

	// 3. `hotbed poll` emits a reminder when events include a command failure (plain + JSON),
	//    and omits it when events have only clean exits or are empty.
	c.eventually("failed command event", func() bool {
		return slices.ContainsFunc(c.events("bright-jay"), func(e *pb.SessionEvent) bool {
			return e.GetCommand().GetWhat() == pb.CommandUpdate_WHAT_EXITED && e.GetCommand().GetExitCode() == 7
		})
	})
	polled := &pb.SessionEvents{}
	c.ok(polled, "poll", "-s", "bright-jay", "--after", "0", "--timeout", "1")
	if !strings.Contains(polled.GetReminder(), "hotbed memory propose") {
		t.Errorf("poll with failed command reminder = %q, want memory propose prompt", polled.GetReminder())
	}
	// Plain text poll also prints the reminder.
	code, pollOut, pollErr := c.hotbed("poll", "-s", "bright-jay", "--after", "0", "--timeout", "1")
	if code != 0 || !strings.Contains(pollOut, "hotbed memory propose") {
		t.Errorf("plain poll with failed command = (%d, %q, %q), want memory reminder", code, pollOut, pollErr)
	}
	// Empty poll carries no reminder.
	emptyPoll := &pb.SessionEvents{}
	c.ok(emptyPoll, "poll", "-s", "bright-jay", "--timeout", "1")
	if emptyPoll.Reminder != nil {
		t.Errorf("empty poll unexpectedly carried reminder: %q", emptyPoll.GetReminder())
	}

	// 4. `question ack` includes the memory-saving nudge alongside the poll reminder.
	c.okIn(`{"questions":[{"question":"Which linter?","header":"Lint","options":[{"label":"oxlint","description":"fast","implemented":true},{"label":"eslint","description":"classic"}]}]}`,
		nil, "question", "add", "-s", "bright-jay")
	c.ok(nil, "question", "answer", "1", "--pick", "oxlint")
	acked := &pb.AckQuestionsResponse{}
	c.ok(acked, "question", "ack", "1", "-s", "bright-jay")
	if !strings.Contains(acked.GetReminder(), "hotbed memory propose") {
		t.Errorf("question ack reminder = %q, want memory propose nudge", acked.GetReminder())
	}
}
