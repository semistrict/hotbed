package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	"github.com/semistrict/hotbed/internal/flame"
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
	// Start a long-running detached command so we can inspect, trace, and profile it live.
	var cmd pb.Command
	c.ok(&cmd, "run", "-s", sess.Id, "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")
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

	// 1b. Activity Monitor-style process sample (JSON envelope & client-formatted plain-text report).
	var sample pb.ProcessSample
	c.ok(&sample, "sample", "-s", sess.Id, "-d", "1", cmd.Id)
	if len(sample.Procs) == 0 || len(sample.Files) == 0 || len(sample.Locks) == 0 || len(sample.SampledStacks) == 0 {
		t.Fatalf("unexpected sample result: procs=%d files=%d locks=%d stacks=%d",
			len(sample.Procs), len(sample.Files), len(sample.Locks), len(sample.SampledStacks))
	}
	code, textSample, stderr := c.hotbed("sample", "-s", sess.Id, "-d", "1", "--text", cmd.Id)
	if code != 0 || strings.Contains(textSample, "=== Diagnosis:") || !strings.Contains(textSample, "=== Process ") || !strings.Contains(textSample, "=== Sampled Stacks") || !strings.Contains(textSample, "main.work") {
		t.Fatalf("sample --text: code=%d out=%q stderr=%q", code, textSample, stderr)
	}
	if want := fmt.Sprintf("hotbed: kept as sample 2 (hotbed samples show %s 2)\n", cmd.Id); !strings.Contains(stderr, want) {
		t.Fatalf("sample --text stderr = %q, want it to say %q", stderr, want)
	}

	// 1c. Each sample is kept with the command, numbered, on the controller.
	c.eventually("both samples kept", func() bool {
		var kept pb.ListCommandSamplesResponse
		c.ok(&kept, "samples", "ls", cmd.Id)
		return len(kept.Samples) == 2
	})
	var kept pb.ListCommandSamplesResponse
	c.ok(&kept, "samples", "ls", cmd.Id)
	for i, s := range kept.Samples {
		if s.CommandId != cmd.Id || s.Seq != int32(i+1) || s.DurationSeconds != 1 || s.By.GetSession() != sess.Id || s.Sample != nil {
			t.Fatalf("samples ls [%d] = %v, want seq %d of %s by %s, 1s, without its sample", i, s, i+1, cmd.Id, sess.Id)
		}
	}
	code, lsText, stderr := c.hotbed("samples", "ls", cmd.Id)
	if code != 0 || !strings.HasPrefix(lsText, "#1     ") || !strings.Contains(lsText, "  1s  all pids  by "+sess.Id+"\n#2 ") {
		t.Fatalf("samples ls text: code=%d out=%q stderr=%q", code, lsText, stderr)
	}
	var shown pb.CommandSample
	c.ok(&shown, "samples", "show", cmd.Id, "1")
	if shown.Seq != 1 || shown.Sample == nil || !proto.Equal(shown.Sample, &sample) {
		t.Fatalf("samples show %s 1 = %v, want the sample the first hotbed sample printed", cmd.Id, &shown)
	}
	code, showText, stderr := c.hotbed("samples", "show", cmd.Id, "2")
	if code != 0 || !strings.HasPrefix(showText, "Sample 2 of "+cmd.Id+", taken ") || !strings.Contains(showText, "=== Sampled Stacks") {
		t.Fatalf("samples show text: code=%d out=%q stderr=%q", code, showText, stderr)
	}
	if code, e := c.fails("samples", "show", cmd.Id, "9"); code != exitError || e.Code != api.CodeNotFound {
		t.Fatalf("samples show of a missing sample: code=%d err=%+v, want not found", code, e)
	}
	var ps pb.ListCommandsResponse
	c.ok(&ps, "ps", "-s", sess.Id)
	if len(ps.Commands) != 1 || ps.Commands[0].Samples != 2 {
		t.Fatalf("ps shows %+v, want the command with 2 samples kept", ps.Commands)
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
		t.Fatalf("expected perf CPU stacks & top functions, got %v", &perfSum)
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
		code, stdout, _ := c.hotbed("--json", "profile", cmd.Id)
		if code != 0 {
			return false
		}
		var out output
		if err := json.Unmarshal([]byte(stdout), &out); err != nil || !out.OK {
			return false
		}
		var p pb.CommandProfile
		if err := protojson.Unmarshal(out.Data, &p); err != nil {
			return false
		}
		// Ensure it's marked non-live once persisted and has samples.
		return !p.Live && p.TotalSamples > 0
	})
	prof.Forget(cmd.Id)

	// Query again after worker's in-memory profile was forgotten: controller SQLite serves it with full filtering!
	var persisted pb.CommandProfile
	c.ok(&persisted, "profile", cmd.Id, "--query", "hotbed.Work")
	if persisted.Live || persisted.TotalSamples != 10 || persisted.MatchedSamples != 8 {
		t.Fatalf("expected persisted profile from controller DB, got live=%v total=%d matched=%d",
			persisted.Live, persisted.TotalSamples, persisted.MatchedSamples)
	}

	// 5. Host-wide node profile via `hotbed profile --node HOST`: includes both
	// command samples and host-only (CommandID == "") samples!
	hostName := nodes.Nodes[0].Hostname
	for i := 0; i < 4; i++ {
		prof.Store.Record(profiler.Sample{
			Timestamp: now.Add(12 * time.Second),
			Comm:      "hotbed-worker",
			Frames: []profiler.ResolvedFrame{
				{Name: "runtime.mcall", Module: "hotbed", FrameType: "go"},
				{Name: "ep_poll", Module: "vmlinux", FrameType: "kernel"},
			},
		})
	}
	var nodeProf pb.CommandProfile
	c.ok(&nodeProf, "profile", "--node", hostName)
	if nodeProf.Node != hostName || !nodeProf.Live || !nodeProf.Active || nodeProf.TotalSamples != 14 || nodeProf.UserSamples != 8 || nodeProf.KernelSamples != 6 {
		t.Fatalf("unexpected node profile: %+v", &nodeProf)
	}
	var nodeFiltered pb.CommandProfile
	c.ok(&nodeFiltered, "profile", "--node", hostName, "--comm", "hotbed-worker", "--last", "5m")
	if nodeFiltered.MatchedSamples != 4 || len(nodeFiltered.Stacks) != 1 || !strings.HasPrefix(flame.Fold(nodeFiltered.Frames, nodeFiltered.Stacks[0]), "hotbed-worker;") {
		t.Fatalf("unexpected filtered node profile: %+v", &nodeFiltered)
	}
	code, nodeTop, _ := c.hotbed("profile", "--node", hostName, "--format", "top")
	if code != 0 || !strings.Contains(nodeTop, "Node: "+hostName) || !strings.Contains(nodeTop, "ep_poll") {
		t.Fatalf("profile --node --format top: code=%d out=%q", code, nodeTop)
	}
	code, nodeFolded, _ := c.hotbed("profile", "--node", hostName, "--format", "folded")
	if code != 0 || !strings.Contains(nodeFolded, "hotbed-worker;runtime.mcall;ep_poll_[k] 4") {
		t.Fatalf("profile --node --format folded: code=%d out=%q", code, nodeFolded)
	}
	_, badProf := c.fails("profile")
	if badProf.Code != api.CodeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS when neither command nor --node is passed, got %+v", badProf)
	}
}

// A command whose stacks are as deep and long-named as a JVM's (TLC's: 150
// frames of 300-character signatures, 40 KB a stack spelled out) profiles
// live with every stack asked for, and its profile reaches the controller
// when it ends, inside the event that says so (board 7115).
func TestProfileOfDeepStacksE2E(t *testing.T) {
	prof := fake.NewProfiler()
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Profiler = prof })
	c.addRepo(upstream(t))
	var sess pb.Session
	c.ok(&sess, "session", "start", "deep-fox")
	c.workspace("deep-fox", nil)
	var cmd pb.Command
	c.ok(&cmd, "run", "-s", sess.Id, "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")

	now := time.Now()
	frame := func(i int) profiler.ResolvedFrame {
		return profiler.ResolvedFrame{
			Name:      fmt.Sprintf("tlc2.tool.TLCState tlc2.tool.impl.Tool.getNextStates%d(tlc2.tool.Action, tla2sany.semantic.OpApplNode, tlc2.tool.impl.ActionItemList, tlc2.util.Context, tlc2.tool.TLCState, tlc2.tool.TLCState, tlc2.tool.INextStateFunctor, tlc2.tool.coverage.CostModel, tla2sany.semantic.ExprOrOpArgNode[], int, int)", i),
			Module:    "tla2tools.jar",
			FrameType: "hotspot",
		}
	}
	for i := range 1000 {
		frames := make([]profiler.ResolvedFrame, 150)
		frames[0], frames[1] = frame(i%400), frame(i/400)
		for d := 2; d < len(frames); d++ {
			frames[d] = frame((i + d*13) % 400)
		}
		prof.Store.Record(profiler.Sample{CommandID: cmd.Id, Timestamp: now, Comm: "java", Frames: frames})
	}

	var live pb.CommandProfile
	c.ok(&live, "profile", cmd.Id)
	if live.TotalSamples != 1000 || len(live.Stacks) != 200 {
		t.Fatalf("live profile has %d samples and %d stacks, want 1000 and 200", live.TotalSamples, len(live.Stacks))
	}
	code, folded, _ := c.hotbed("profile", cmd.Id, "--format", "folded")
	if lines := strings.Split(strings.TrimSpace(folded), "\n"); code != 0 || len(lines) != 200 || strings.Count(lines[0], ";") != 149 {
		t.Fatalf("profile --format folded: code=%d, %d lines", code, len(lines))
	}

	c.ok(nil, "stop", "-s", sess.Id, cmd.Id)
	c.eventually("profile persisted on controller", func() bool {
		_, ok, err := c.store.CommandProfile(t.Context(), cmd.Id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	})
	var persisted pb.CommandProfile
	c.ok(&persisted, "profile", cmd.Id, "--limit", "500")
	if persisted.Live || persisted.TotalSamples != 1000 || len(persisted.Stacks) != 500 {
		t.Fatalf("persisted profile: live=%v, %d samples, %d stacks; want ended, 1000, 500", persisted.Live, persisted.TotalSamples, len(persisted.Stacks))
	}
}

// A worker keeps a finished command's profile file only until the
// controller has the profile: once it has applied the event that carried
// it, the worker removes the file when it forgets the command, and the
// controller serves the profile on its own (board 7183).
func TestWorkerRemovesDeliveredProfileFiles(t *testing.T) {
	prof := fake.NewProfiler()
	workerClock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Profiler, cfg.Now = prof, workerClock.Now })
	c.addRepo(upstream(t))
	var sess pb.Session
	c.ok(&sess, "session", "start", "tidy-fox")
	c.workspace("tidy-fox", nil)
	var cmd pb.Command
	c.ok(&cmd, "run", "-s", sess.Id, "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")
	prof.Store.Record(profiler.Sample{CommandID: cmd.Id, Timestamp: time.Now(), Comm: "app", Frames: []profiler.ResolvedFrame{{Name: "main.main", FrameType: "go"}}})
	c.ok(nil, "stop", "-s", sess.Id, cmd.Id)

	file := filepath.Join(c.nodes[0].dir, "profiles", cmd.Id+".pb.gz")
	c.eventually("the controller to have the profile", func() bool {
		_, ok, err := c.store.CommandProfile(t.Context(), cmd.Id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	})
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("profile file before the worker forgets its command: %v", err)
	}
	workerClock.advance(3 * time.Minute)
	c.eventually("its worker to forget it", func() bool {
		_, ok := c.engine().Command(cmd.Id)
		return !ok
	})
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile file after the worker forgot its command: %v, want it gone", err)
	}
	var p pb.CommandProfile
	c.ok(&p, "profile", cmd.Id)
	if p.TotalSamples != 1 || len(p.Stacks) != 1 {
		t.Fatalf("profile after the worker forgot it: %d samples, %d stacks; want the controller's 1 and 1", p.TotalSamples, len(p.Stacks))
	}
}

// rawCommands that emit structured data (`run --detach`, `trace` summary,
// `sample`, and `profile`) honor `--jq`, `--json`, and default plain-text
// output consistently (board 7894).
func TestFormatRawCommandsHonorJSONJQAndPlainText(t *testing.T) {
	prof := fake.NewProfiler()
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.Inspector = fake.Inspector{}
		cfg.Profiler = prof
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "fmt-fox")
	c.workspace("fmt-fox", nil)

	// 1. run --detach with --jq, plain text, and --json
	code, jqID, stderr := c.hotbed("--jq", ".id", "run", "-s", "fmt-fox", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")
	cmdID := strings.TrimSpace(jqID)
	if code != 0 || cmdID == "" || strings.HasPrefix(cmdID, "{") || strings.HasPrefix(cmdID, "\"") {
		t.Fatalf("run --detach --jq .id: code=%d out=%q stderr=%q", code, jqID, stderr)
	}
	code, plainRun, stderr := c.hotbed("run", "-s", "fmt-fox", "-r", "app", "--detach", "--mem", "1G", "--", "true")
	if code != 0 || strings.HasPrefix(strings.TrimSpace(plainRun), "{") || !strings.Contains(plainRun, "id") {
		t.Fatalf("run --detach plain text: code=%d out=%q stderr=%q", code, plainRun, stderr)
	}

	c.eventually("command running", func() bool {
		var ps pb.ListCommandsResponse
		c.ok(&ps, "ps", "-s", "fmt-fox")
		for _, item := range ps.Commands {
			if item.Id == cmdID && item.State == pb.CommandState_COMMAND_STATE_RUNNING {
				return true
			}
		}
		return false
	})

	prof.Store.Record(profiler.Sample{
		CommandID: cmdID,
		Timestamp: time.Now(),
		Comm:      "sleep",
		Frames:    []profiler.ResolvedFrame{{Name: "main.work", FrameType: "go"}},
	})

	// 2. sample with --jq and plain text
	code, jqSample, stderr := c.hotbed("--jq", ".procs[0].comm", "sample", "-s", "fmt-fox", "-d", "1", cmdID)
	if code != 0 || strings.TrimSpace(jqSample) != "fake-proc" {
		t.Fatalf("sample --jq: code=%d out=%q stderr=%q", code, jqSample, stderr)
	}
	code, plainSample, stderr := c.hotbed("sample", "-s", "fmt-fox", "-d", "1", cmdID)
	if code != 0 || strings.HasPrefix(strings.TrimSpace(plainSample), "{") || !strings.Contains(plainSample, "=== Process ") || !strings.Contains(plainSample, "fake-proc") {
		t.Fatalf("sample plain text: code=%d out=%q stderr=%q", code, plainSample, stderr)
	}

	// 3. trace summary with --jq and plain text
	code, jqTrace, stderr := c.hotbed("--jq", ".syscalls[0].name", "trace", "strace", "-s", "fmt-fox", "--summary", "-d", "1", cmdID)
	if code != 0 || strings.TrimSpace(jqTrace) != "futex" {
		t.Fatalf("trace strace --summary --jq: code=%d out=%q stderr=%q", code, jqTrace, stderr)
	}
	code, plainTrace, stderr := c.hotbed("trace", "strace", "-s", "fmt-fox", "--summary", "-d", "1", cmdID)
	if code != 0 || strings.HasPrefix(strings.TrimSpace(plainTrace), "{") || !strings.Contains(plainTrace, "futex") {
		t.Fatalf("trace strace --summary plain text: code=%d out=%q stderr=%q", code, plainTrace, stderr)
	}

	// 4. profile with --jq and plain text
	code, jqProf, stderr := c.hotbed("--jq", ".totalSamples", "profile", cmdID)
	if code != 0 || strings.TrimSpace(jqProf) != "1" {
		t.Fatalf("profile --jq: code=%d out=%q stderr=%q", code, jqProf, stderr)
	}
	code, plainProf, stderr := c.hotbed("profile", cmdID)
	if code != 0 || strings.HasPrefix(strings.TrimSpace(plainProf), "{") || !strings.Contains(plainProf, "totalSamples") {
		t.Fatalf("profile plain text: code=%d out=%q stderr=%q", code, plainProf, stderr)
	}

	c.ok(nil, "stop", "-s", "fmt-fox", cmdID)
}

// Explicit `--format json` on `profile` (and `trace perf`) must emit valid JSON
// even without global `--json`, while still honoring `--jq` and leaving the
// omitted `--format` default as human-readable plain text (board 8295).
func TestProfileExplicitFormatJSONAndPlainText(t *testing.T) {
	prof := fake.NewProfiler()
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.Inspector = fake.Inspector{}
		cfg.Profiler = prof
	})
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "prof-fmt-fox")
	c.workspace("prof-fmt-fox", nil)

	var cmd pb.Command
	c.ok(&cmd, "run", "-s", "prof-fmt-fox", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "30")
	c.eventually("command running", func() bool {
		var ps pb.ListCommandsResponse
		c.ok(&ps, "ps", "-s", "prof-fmt-fox")
		for _, item := range ps.Commands {
			if item.Id == cmd.Id && item.State == pb.CommandState_COMMAND_STATE_RUNNING {
				return true
			}
		}
		return false
	})

	prof.Store.Record(profiler.Sample{
		CommandID: cmd.Id,
		Timestamp: time.Now(),
		Comm:      "sleep",
		Frames:    []profiler.ResolvedFrame{{Name: "main.work", FrameType: "go"}},
	})

	t.Run("profile --format json emits valid JSON without global --json", func(t *testing.T) {
		code, stdout, stderr := c.hotbed("profile", cmd.Id, "--format", "json")
		if code != 0 {
			t.Fatalf("profile --format json: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		var env output
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("profile --format json stdout is not valid JSON (%v): %q", err, stdout)
		}
		if !env.OK || env.Command != "profile" {
			t.Fatalf("unexpected envelope: %+v", env)
		}
		var got pb.CommandProfile
		if err := protojson.Unmarshal(env.Data, &got); err != nil {
			t.Fatalf("unmarshal CommandProfile: %v", err)
		}
		if got.TotalSamples != 1 {
			t.Fatalf("totalSamples = %d, want 1", got.TotalSamples)
		}
	})

	t.Run("profile --format json still honors --jq", func(t *testing.T) {
		code, stdout, stderr := c.hotbed("--jq", ".totalSamples", "profile", cmd.Id, "--format", "json")
		if code != 0 || strings.TrimSpace(stdout) != "1" {
			t.Fatalf("profile --jq --format json: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("profile without --format or --json emits plain text", func(t *testing.T) {
		code, stdout, stderr := c.hotbed("profile", cmd.Id)
		if code != 0 {
			t.Fatalf("profile plain: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		trimmed := strings.TrimSpace(stdout)
		if strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "totalSamples") {
			t.Fatalf("profile without --format/--json should emit plain text, got %q", stdout)
		}
	})

	t.Run("trace perf --format json emits valid JSON and omitted emits plain text", func(t *testing.T) {
		code, stdout, stderr := c.hotbed("trace", "perf", "-s", "prof-fmt-fox", "-d", "1", "--format", "json", cmd.Id)
		if code != 0 {
			t.Fatalf("trace perf --format json: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		var env output
		if err := json.Unmarshal([]byte(stdout), &env); err != nil || !env.OK {
			t.Fatalf("trace perf --format json stdout is not valid JSON (%v): %q", err, stdout)
		}
		code, plainOut, stderr := c.hotbed("trace", "perf", "-s", "prof-fmt-fox", "-d", "1", cmd.Id)
		if code != 0 || strings.HasPrefix(strings.TrimSpace(plainOut), "{") || !strings.Contains(plainOut, "topFunctions") {
			t.Fatalf("trace perf plain: code=%d stdout=%q stderr=%q", code, plainOut, stderr)
		}
	})

	c.ok(nil, "stop", "-s", "prof-fmt-fox", cmd.Id)
}

func TestFormatSampleTextReportGroupedStacksAndBoundsCheck(t *testing.T) {
	t.Run("out-of-bounds frame indices do not panic", func(t *testing.T) {
		sample := &pb.ProcessSample{
			CommandId: "cmd-oob",
			Hostname:  "ubu1",
			Arch:      "amd64",
			Kernel:    "6.12.0",
			Frames: []*pb.ProfileFrame{
				{Name: "app", FrameType: "process"},
				{Name: "main.work", FrameType: "go"},
			},
			SampledStacks: []*pb.FoldedStack{
				{Comm: "app", Frames: []uint32{0, 1, 999}, Samples: 4},
			},
			OnCpuTopFunctions: []*pb.ProfileFunction{
				{Frame: 1, SelfPct: 60.0, TotalPct: 100.0},
				{Frame: 42, SelfPct: 40.0, TotalPct: 40.0},
			},
		}
		report := formatSampleTextReport(sample)
		if !strings.Contains(report, "main.work;[unknown#999]") {
			t.Fatalf("expected safe folded stack with unknown placeholder, got:\n%s", report)
		}
		if !strings.Contains(report, "[go    ] main.work") {
			t.Fatalf("expected valid top function in report, got:\n%s", report)
		}
		if !strings.Contains(report, "[unknown] [unknown#42]") {
			t.Fatalf("expected out-of-bounds top function fallback in report, got:\n%s", report)
		}
	})

	t.Run("sampled stacks are grouped by comm with per-comm percentages", func(t *testing.T) {
		sample := &pb.ProcessSample{
			CommandId: "cmd-group",
			Hostname:  "ubu1",
			Frames: []*pb.ProfileFrame{
				{Name: "rustc", FrameType: "process"},
				{Name: "rustc_codegen::compile", FrameType: "native"},
				{Name: "rustc_borrowck::check", FrameType: "native"},
				{Name: "cargo", FrameType: "process"},
				{Name: "cargo::ops::compile", FrameType: "native"},
			},
			SampledStacks: []*pb.FoldedStack{
				{Comm: "cargo", Frames: []uint32{3, 4}, Samples: 5},
				{Comm: "rustc", Frames: []uint32{0, 1}, Samples: 15},
				{Comm: "rustc", Frames: []uint32{0, 2}, Samples: 5},
			},
		}
		report := formatSampleTextReport(sample)
		rustcHeader := "Process rustc (20 samples, 2 unique stacks):"
		cargoHeader := "Process cargo (5 samples, 1 unique stacks):"
		rustcIdx := strings.Index(report, rustcHeader)
		cargoIdx := strings.Index(report, cargoHeader)
		if rustcIdx < 0 || cargoIdx < 0 {
			t.Fatalf("missing per-comm headers in report:\n%s", report)
		}
		if rustcIdx > cargoIdx {
			t.Fatalf("expected higher-sample comm (rustc) before cargo:\n%s", report)
		}
		if !strings.Contains(report, "  15 ( 75.0%)  rustc;rustc_codegen::compile") {
			t.Fatalf("expected 75.0%% of rustc samples for compile, got:\n%s", report)
		}
		if !strings.Contains(report, "   5 ( 25.0%)  rustc;rustc_borrowck::check") {
			t.Fatalf("expected 25.0%% of rustc samples for borrowck, got:\n%s", report)
		}
		if !strings.Contains(report, "   5 (100.0%)  cargo;cargo::ops::compile") {
			t.Fatalf("expected 100.0%% of cargo samples, got:\n%s", report)
		}
	})
}
