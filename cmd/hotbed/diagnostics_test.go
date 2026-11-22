package main

import (
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/worker"
)

func TestInspectTraceAndContinuousProfileE2E(t *testing.T) {
	prof := fake.NewProfiler()
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.Inspector = fake.Inspector{}
		cfg.Profiler = prof
	})
	origin := upstream(t)
	c.addRepo(origin)

	// Verify node heartbeat advertises the always-on OTel eBPF profiler.
	c.heartbeat()
	var nodes pb.ListNodesResponse
	c.ok(&nodes, "nodes")
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Profiler == nil || !nodes.Nodes[0].Profiler.Active || nodes.Nodes[0].Profiler.Engine != profiler.EngineName {
		t.Fatalf("expected active OTel profiler in hotbed nodes, got %+v", nodes.Nodes)
	}

	var sess pb.Session
	c.ok(&sess, "session", "start", "trace-fox")
	c.ok(nil, "add", "-s", sess.Id, "-r", "app")

	// Start a long-running detached command so we can inspect, trace, and profile it live.
	var cmd pb.Command
	c.ok(&cmd, "run", "-s", sess.Id, "-r", "app", "--detach", "--", "sleep", "30")
	c.eventually("command running", func() bool {
		var ps pb.ListCommandsResponse
		c.ok(&ps, "ps", "-s", sess.Id)
		for _, item := range ps.Commands {
			if item.Id == cmd.Id && item.State == pb.CommandState_COMMAND_STATE_RUNNING {
				return true
			}
		}
		return false
	})

	// 1. Opt-in Linux inspection: all sections & single sections.
	var full pb.InspectCommandResponse
	c.ok(&full, "inspect", "-s", sess.Id, cmd.Id)
	if len(full.Procs) == 0 || len(full.Files) == 0 || full.CgroupStats == nil || len(full.Kstacks) == 0 || len(full.Maps) == 0 || len(full.Limits) == 0 {
		t.Fatalf("expected all inspect sections populated, got procs=%d files=%d cgroup=%v kstacks=%d maps=%d limits=%d",
			len(full.Procs), len(full.Files), full.CgroupStats != nil, len(full.Kstacks), len(full.Maps), len(full.Limits))
	}
	var onlyFiles pb.InspectCommandResponse
	c.ok(&onlyFiles, "inspect", "files", "-s", sess.Id, cmd.Id)
	if len(onlyFiles.Files) == 0 || len(onlyFiles.Procs) != 0 {
		t.Fatalf("expected only files section, got files=%d procs=%d", len(onlyFiles.Files), len(onlyFiles.Procs))
	}

	// 2. Opt-in active tracers: strace live, strace -c, perf CPU (json & folded), perf stat, stacks, bpf.
	code, out, stderr := c.hotbed("trace", "strace", "-s", sess.Id, "-d", "1", cmd.Id)
	if code != 0 || !strings.Contains(out, "openat(") {
		t.Fatalf("trace strace: code=%d out=%q stderr=%q", code, out, stderr)
	}
	var straceSum pb.TraceSummary
	c.ok(&straceSum, "trace", "strace", "-s", sess.Id, "--summary", "-d", "1", cmd.Id)
	if len(straceSum.Syscalls) == 0 || straceSum.Syscalls[0].Name != "futex" {
		t.Fatalf("expected strace summary syscalls, got %+v", straceSum.Syscalls)
	}
	var perfSum pb.TraceSummary
	c.ok(&perfSum, "trace", "perf", "-s", sess.Id, "-d", "1", cmd.Id)
	if len(perfSum.Stacks) == 0 || len(perfSum.TopFunctions) == 0 {
		t.Fatalf("expected perf CPU stacks & top functions, got %+v", perfSum)
	}
	code, foldedOut, _ := c.hotbed("trace", "perf", "-s", sess.Id, "-d", "1", "--format", "folded", cmd.Id)
	if code != 0 || !strings.Contains(foldedOut, "fake-proc;main.main;main.work 42") {
		t.Fatalf("trace perf --format folded: code=%d out=%q", code, foldedOut)
	}
	var statSum pb.TraceSummary
	c.ok(&statSum, "trace", "stat", "-s", sess.Id, "-d", "1", cmd.Id)
	if len(statSum.Counters) < 2 {
		t.Fatalf("expected perf stat counters, got %+v", statSum.Counters)
	}
	var stacksSum pb.TraceSummary
	c.ok(&stacksSum, "trace", "stacks", "-s", sess.Id, cmd.Id)
	if stacksSum.Raw == "" {
		t.Fatalf("expected stacks raw output")
	}
	var bpfSum pb.TraceSummary
	c.ok(&bpfSum, "trace", "bpf", "-s", sess.Id, "--probe", "vfsstat", "-d", "1", cmd.Id)
	if bpfSum.Raw == "" {
		t.Fatalf("expected bpf vfsstat raw output")
	}

	// 3. Always-on OpenTelemetry eBPF continuous profile (live query).
	now := time.Now()
	for i := 0; i < 8; i++ {
		prof.Store.Record(profiler.Sample{
			CommandID: cmd.Id,
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Comm:      "worker.test",
			Frames: []profiler.ResolvedFrame{
				{Name: "testing.tRunner", Module: "worker.test", FrameType: "go"},
				{Name: "hotbed.Work", Module: "worker.test", FrameType: "go"},
			},
		})
	}
	for i := 0; i < 2; i++ {
		prof.Store.Record(profiler.Sample{
			CommandID: cmd.Id,
			Timestamp: now.Add(6 * time.Second),
			Comm:      "worker.test",
			Frames: []profiler.ResolvedFrame{
				{Name: "syscall.Syscall", Module: "worker.test", FrameType: "go"},
				{Name: "vfs_write", Module: "vmlinux", FrameType: "kernel"},
			},
		})
	}

	var liveProf pb.CommandProfile
	c.ok(&liveProf, "profile", cmd.Id)
	if !liveProf.Live || !liveProf.Active || liveProf.TotalSamples != 10 || liveProf.UserSamples != 8 || liveProf.KernelSamples != 2 {
		t.Fatalf("unexpected live profile: %+v", &liveProf)
	}

	// Filter query + folded + top text formats.
	var qProf pb.CommandProfile
	c.ok(&qProf, "profile", cmd.Id, "--query", "vfs_write", "--type", "kernel")
	if qProf.MatchedSamples != 2 || len(qProf.Stacks) != 1 {
		t.Fatalf("unexpected filtered profile: %+v", &qProf)
	}
	code, topOut, _ := c.hotbed("profile", cmd.Id, "--format", "top")
	if code != 0 || !strings.Contains(topOut, "hotbed.Work") {
		t.Fatalf("profile --format top: code=%d out=%q", code, topOut)
	}
	code, foldedProf, _ := c.hotbed("profile", cmd.Id, "--format", "folded")
	if code != 0 || !strings.Contains(foldedProf, "testing.tRunner;hotbed.Work 8") {
		t.Fatalf("profile --format folded: code=%d out=%q", code, foldedProf)
	}

	// 4. Stop command -> profile is persisted in controller SQLite and remains queryable after exit!
	c.ok(nil, "stop", "-s", sess.Id, cmd.Id)

	// Inspecting an ended command fails cleanly with CONFLICT and suggests hotbed profile.
	_, errObj := c.fails("inspect", "-s", sess.Id, cmd.Id)
	if errObj.Code != api.CodeConflict || !strings.Contains(errObj.Suggestion, "hotbed profile") {
		t.Fatalf("expected CONFLICT suggesting hotbed profile, got %+v", errObj)
	}

	// Wait for the command's exit event + profile to reach the controller DB, then forget it from the worker's in-memory profiler!
	c.eventually("profile persisted on controller", func() bool {
		var p pb.CommandProfile
		code, stdout, _ := c.hotbed("profile", cmd.Id)
		if code != 0 {
			return false
		}
		// Ensure it's marked non-live once persisted.
		return strings.Contains(stdout, `"live": false`) || strings.Contains(stdout, `"live":false`) || !p.Live
	})
	prof.Forget(cmd.Id)

	// Query again after worker's in-memory profile was forgotten: controller SQLite serves it with full filtering!
	var persisted pb.CommandProfile
	c.ok(&persisted, "profile", cmd.Id, "--query", "hotbed.Work")
	if persisted.Live || persisted.TotalSamples != 10 || persisted.MatchedSamples != 8 {
		t.Fatalf("expected persisted profile from controller DB, got live=%v total=%d matched=%d",
			persisted.Live, persisted.TotalSamples, persisted.MatchedSamples)
	}
}
