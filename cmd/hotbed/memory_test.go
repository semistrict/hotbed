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
	c.ok(nil, "session", "start", "brave-otter")
	c.ok(nil, "session", "start", "calm-heron")

	// Agents cannot self-approve a memory.
	if _, e := c.fails("memory", "propose", "-s", "brave-otter", "--global", "--mode", "always",
		"--slug", "no-self-approve", "--summary", "Should fail", "--approve"); e.Code != api.CodeInvalidArgs {
		t.Fatalf("self-approve by agent = %v, want INVALID_ARGS", e)
	}

	// Propose a global always-on rule (the triggering example).
	prop1 := &pb.Memory{}
	c.ok(prop1, "memory", "propose", "-s", "brave-otter",
		"--global", "--mode", "always",
		"--slug", "subagent-model-neon",
		"--summary", "Use gemini-4-neon (not GPT models) for all T3 Code delegated subagents.",
		"--rationale", "User correction in thread fxdax",
		"--model", "gemini-4-neon",
		"--tag", "subagents", "--tag", "models",
	)
	if prop1.Id != 1 || prop1.State != pb.MemoryState_MEMORY_STATE_PROPOSED || prop1.Scope != pb.MemoryScope_MEMORY_SCOPE_GLOBAL {
		t.Fatalf("prop1 = %v", prop1)
	}
	if prop1.CockpitUrl == "" || !strings.HasSuffix(prop1.CockpitUrl, "/memories/1") {
		t.Errorf("prop1 cockpitUrl = %q, want suffix /memories/1", prop1.CockpitUrl)
	}
	if prop1.GetReminder() == "" {
		t.Errorf("expected poll reminder on proposal")
	}

	// Propose a project-scoped on-demand memory with --body-file - (stdin).
	prop2 := &pb.Memory{}
	c.okIn("# Debugging ZFS flakes\n\nRun `zpool status` first.\n", prop2,
		"memory", "propose", "-s", "brave-otter",
		"--slug", "zfs-base-freshen",
		"--title", "ZFS base freshening",
		"--summary", "How to debug ZFS snapshot/clone base freshening flakes.",
		"--body-file", "-",
	)
	if prop2.Id != 2 || prop2.Scope != pb.MemoryScope_MEMORY_SCOPE_PROJECT || prop2.Project != "test" {
		t.Fatalf("prop2 = %v", prop2)
	}

	// Edit proposal #2 before approval.
	edited2 := &pb.Memory{}
	c.ok(edited2, "memory", "edit", "-s", "calm-heron", "2",
		"--summary", "How to debug ZFS snapshot/clone base freshening flakes (read before debugging).",
		"--if-revision", fmt.Sprint(prop2.Revision),
	)
	if edited2.Revision != prop2.Revision+1 {
		t.Errorf("edited2 rev = %d, want %d", edited2.Revision, prop2.Revision+1)
	}

	// Proposals list shows both; default `memory ls` (approved) shows neither yet.
	props := &pb.ListMemoriesResponse{}
	c.ok(props, "memory", "proposals", "-s", "brave-otter")
	if len(props.Memories) != 2 {
		t.Fatalf("proposals = %d, want 2", len(props.Memories))
	}
	approvedList := &pb.ListMemoriesResponse{}
	c.ok(approvedList, "memory", "ls", "-s", "brave-otter")
	if len(approvedList.Memories) != 0 {
		t.Fatalf("approved ls before approval = %d, want 0", len(approvedList.Memories))
	}

	// Snapshot reports pending memories (1 global, 1 project = 2 total).
	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	if snap.PendingMemories != 2 || snap.GlobalPendingMemories != 1 {
		t.Errorf("snapshot pendingMemories=%d global=%d, want 2 and 1", snap.PendingMemories, snap.GlobalPendingMemories)
	}

	// User approves #1 as-is and #2 with edits.
	app1 := &pb.Memory{}
	c.ok(app1, "memory", "approve", "1")
	if app1.State != pb.MemoryState_MEMORY_STATE_APPROVED {
		t.Fatalf("app1 state = %v", app1.State)
	}
	app2 := &pb.Memory{}
	c.ok(app2, "memory", "approve", "2", "--summary", "ZFS base freshening playbook — read before debugging base failures.")
	if app2.State != pb.MemoryState_MEMORY_STATE_APPROVED {
		t.Fatalf("app2 state = %v", app2.State)
	}

	// Proposer's poll receives both approval events.
	polled := &pb.SessionEvents{}
	c.ok(polled, "poll", "-s", "brave-otter", "--timeout", "1")
	var memWhats []pb.MemoryUpdate_What
	for _, ev := range polled.Events {
		if m := ev.GetMemory(); m != nil {
			memWhats = append(memWhats, m.What)
		}
	}
	wantWhats := []pb.MemoryUpdate_What{
		pb.MemoryUpdate_WHAT_APPROVED,
		pb.MemoryUpdate_WHAT_APPROVED_EDITED,
	}
	if !slices.Equal(memWhats, wantWhats) {
		t.Fatalf("poll memory events = %v, want %v", memWhats, wantWhats)
	}

	// Starting a new session returns both memories in compact form (plain text + JSON).
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

	// `memory show` returns full audit history.
	shown := &pb.Memory{}
	c.ok(shown, "memory", "show", "-s", "keen-fox", "2")
	if len(shown.History) < 3 {
		t.Errorf("memory show 2 history len = %d, want >= 3 (proposed, edited, approved)", len(shown.History))
	}

	// Superseding an existing memory atomically transitions old -> superseded and keeps slug working.
	prop3 := &pb.Memory{}
	c.ok(prop3, "memory", "propose", "-s", "brave-otter",
		"--global", "--mode", "always",
		"--slug", "subagent-model-neon",
		"--supersede", "1",
		"--summary", "Use gemini-4-neon (high) for all T3 Code subagents; never GPT.",
	)
	app3 := &pb.Memory{}
	c.ok(app3, "memory", "approve", fmt.Sprint(prop3.Id))
	if app3.State != pb.MemoryState_MEMORY_STATE_APPROVED {
		t.Fatalf("app3 state = %v", app3.State)
	}
	old1 := &pb.Memory{}
	c.ok(old1, "memory", "show", "1")
	if old1.State != pb.MemoryState_MEMORY_STATE_SUPERSEDED || old1.SupersededBy != prop3.Id {
		t.Fatalf("old1 after supersede = state=%v supersededBy=%d", old1.State, old1.SupersededBy)
	}
	// Slug resolves to the new #3!
	gotNew := &pb.GetMemoriesResponse{}
	c.ok(gotNew, "memory", "get", "subagent-model-neon")
	if len(gotNew.Memories) != 1 || gotNew.Memories[0].Id != prop3.Id {
		t.Fatalf("slug lookup after supersede = %+v, want #%d", gotNew.Memories, prop3.Id)
	}

	// User revise, move, archive, restore, reject, and agent withdraw (also ensures 100% RPC CLI coverage!).
	rev := &pb.Memory{}
	c.ok(rev, "memory", "revise", fmt.Sprint(prop3.Id), "--summary", "Use gemini-4-neon for subagents (never GPT).")
	if !strings.Contains(rev.Content.Summary, "never GPT") {
		t.Errorf("revised summary = %q", rev.Content.Summary)
	}

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

	// Propose + reject delivers poll event to proposer.
	prop4 := &pb.Memory{}
	c.ok(prop4, "memory", "propose", "-s", "brave-otter", "--global", "--mode", "always",
		"--slug", "bad-rule", "--summary", "A rule to reject")
	rej := &pb.Memory{}
	c.ok(rej, "memory", "reject", fmt.Sprint(prop4.Id), "--reason", "too broad")
	if rej.State != pb.MemoryState_MEMORY_STATE_REJECTED {
		t.Fatalf("rejected state = %v", rej.State)
	}
	polledRej := &pb.SessionEvents{}
	c.ok(polledRej, "poll", "-s", "brave-otter", "--timeout", "1")
	if !slices.ContainsFunc(polledRej.Events, func(e *pb.SessionEvent) bool {
		return e.GetMemory().GetWhat() == pb.MemoryUpdate_WHAT_REJECTED && e.GetMemory().GetMemory().GetId() == prop4.Id
	}) {
		t.Errorf("expected WHAT_REJECTED poll event for #%d, got %+v", prop4.Id, polledRej.Events)
	}

	// Propose + withdraw by agent.
	prop5 := &pb.Memory{}
	c.ok(prop5, "memory", "propose", "-s", "brave-otter", "--global", "--mode", "always",
		"--slug", "temp-draft", "--summary", "Draft to withdraw")
	wd := &pb.Memory{}
	c.ok(wd, "memory", "withdraw", "-s", "brave-otter", fmt.Sprint(prop5.Id), "--reason", "superseded by better idea")
	if wd.State != pb.MemoryState_MEMORY_STATE_WITHDRAWN {
		t.Fatalf("withdrawn state = %v", wd.State)
	}
}

func TestMCPServerIncludesMemoriesInInstructionsAndTools(t *testing.T) {
	c := newCluster(t)
	// Seed an approved global memory directly (user action, no session bound).
	c.ok(nil, "memory", "propose", "--global", "--mode", "always",
		"--slug", "mcp-test-rule", "--summary", "Always prefer neon over GPT.", "--approve")

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("T3CODE_THREAD_ID", "thread-mcp-mem")
	m := c.mcp()
	defer m.stop()

	instr := m.InitializeResult().Instructions
	if !strings.Contains(instr, "mcp-test-rule") || !strings.Contains(instr, "Always prefer neon over GPT.") {
		t.Errorf("MCP server instructions missing approved memory:\n%s", instr)
	}

	// Fetch via memory_get MCP tool.
	got := &pb.GetMemoriesResponse{}
	m.call(got, "memory_get", map[string]any{"ref": []string{"mcp-test-rule"}})
	if len(got.Memories) != 1 || got.Memories[0].Content.Slug != "mcp-test-rule" {
		t.Fatalf("MCP memory_get = %+v", got)
	}

	// Propose via memory_propose MCP tool (inherits MCP session automatically).
	prop := &pb.Memory{}
	m.call(prop, "memory_propose", map[string]any{
		"global":  true,
		"mode":    "on_demand",
		"slug":    "mcp-playbook",
		"summary": "Playbook proposed over MCP",
		"body":    "Step 1: verify MCP.\n",
	})
	if prop.State != pb.MemoryState_MEMORY_STATE_PROPOSED || prop.Proposed.Session == "" {
		t.Fatalf("MCP memory_propose = %+v", prop)
	}
	_ = filepath.Clean(dir)
}
