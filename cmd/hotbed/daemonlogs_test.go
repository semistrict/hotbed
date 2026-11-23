package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

func TestDaemonLogsCLIAndMultiNodeAggregation(t *testing.T) {
	now := time.Now().UTC()
	ctrlLogs, err := daemonlog.Open(filepath.Join(t.TempDir(), "ctrl-logs"), daemonlog.Config{
		MaxFileBytes: 4096,
		MaxBackups:   2,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrlLogs.Close() })

	workerLogs := map[string]*daemonlog.Store{}
	for _, host := range []string{"asahimbp", "mini"} {
		st, err := daemonlog.Open(filepath.Join(t.TempDir(), host+"-logs"), daemonlog.Config{
			MaxFileBytes: 4096,
			MaxBackups:   2,
			Now:          func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		workerLogs[host] = st
	}

	c := newFleetWith(t, 2, clusterOpts{
		server: func(cfg *controller.ServerConfig) {
			cfg.DaemonLogs = ctrlLogs
		},
		engine: []func(*worker.EngineConfig){
			func(cfg *worker.EngineConfig) {
				cfg.DaemonLogs = workerLogs[cfg.Host]
			},
		},
	})
	c.online()

	// Emit INFO (ignored by daemon logs), WARN, and ERROR on controller and each worker.
	ctrlLogger := slog.New(ctrlLogs.Handler(nil))
	ctrlLogger.Info("controller info ignored")
	ctrlLogger.Warn("controller warn sample", "component", "reaper")
	time.Sleep(5 * time.Millisecond)
	slog.New(workerLogs["asahimbp"].Handler(nil)).Error("asahi error sample", "err", "zfs busy")
	time.Sleep(5 * time.Millisecond)
	slog.New(workerLogs["mini"].Handler(nil)).Warn("mini warn sample", "volume", "ws-99")

	// Default: all sources combined, newest first.
	all := &pb.GetDaemonLogsResponse{}
	c.ok(all, "daemon-logs")
	if len(all.Entries) != 3 {
		t.Fatalf("daemon-logs got %d entries, want 3: %+v", len(all.Entries), all.Entries)
	}
	if all.Entries[0].Source != "mini" || all.Entries[1].Source != "asahimbp" || all.Entries[2].Source != "controller" {
		t.Errorf("entries order = [%s, %s, %s], want [mini, asahimbp, controller]",
			all.Entries[0].Source, all.Entries[1].Source, all.Entries[2].Source)
	}
	if len(all.Sources) != 3 || all.Sources[0].Role != "controller" {
		t.Fatalf("sources = %+v, want controller + 2 workers", all.Sources)
	}
	for _, src := range all.Sources {
		if !src.Reachable {
			t.Errorf("source %s unexpectedly unreachable: %s", src.Source, src.Error)
		}
		if src.MaxFiles != 3 || src.MaxFileBytes != 4096 {
			t.Errorf("source %s rotation stats = %+v", src.Source, src)
		}
	}

	// Plain text rendering includes timestamps, levels, sources, messages, and attrs.
	code, plain, stderr := c.hotbed("daemon-logs")
	if code != exitOK {
		t.Fatalf("daemon-logs plain exited %d: %s", code, stderr)
	}
	for _, want := range []string{"WARN", "ERROR", "controller", "asahimbp", "mini", "asahi error sample", "zfs busy"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain output missing %q:\n%s", want, plain)
		}
	}

	// Filter by --source controller and --source asahimbp.
	ctrlOnly := &pb.GetDaemonLogsResponse{}
	c.ok(ctrlOnly, "daemon-logs", "--source", "controller")
	if len(ctrlOnly.Entries) != 1 || ctrlOnly.Entries[0].Msg != "controller warn sample" {
		t.Fatalf("--source controller = %+v", ctrlOnly.Entries)
	}

	asahiOnly := &pb.GetDaemonLogsResponse{}
	c.ok(asahiOnly, "daemon-logs", "--source", "asahimbp")
	if len(asahiOnly.Entries) != 1 || asahiOnly.Entries[0].Msg != "asahi error sample" {
		t.Fatalf("--source asahimbp = %+v", asahiOnly.Entries)
	}

	// Filter by --level error and -q query and --since.
	errOnly := &pb.GetDaemonLogsResponse{}
	c.ok(errOnly, "daemon-logs", "--level", "error", "--since", "1h")
	if len(errOnly.Entries) != 1 || errOnly.Entries[0].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR {
		t.Fatalf("--level error = %+v", errOnly.Entries)
	}
	qOnly := &pb.GetDaemonLogsResponse{}
	c.ok(qOnly, "daemon-logs", "-q", "reaper")
	if len(qOnly.Entries) != 1 || qOnly.Entries[0].Source != "controller" {
		t.Fatalf("-q reaper = %+v", qOnly.Entries)
	}

	// Unknown source returns NOT_FOUND.
	if _, e := c.fails("daemon-logs", "--source", "ghost-node"); e.Code != api.CodeNotFound {
		t.Errorf("--source ghost-node = %v, want NOT_FOUND", e)
	}

	// Invalid --since returns INVALID_ARGS.
	if _, e := c.fails("daemon-logs", "--since", "not-a-duration"); e.Code != api.CodeInvalidArgs {
		t.Errorf("--since invalid = %v, want INVALID_ARGS", e)
	}

	// Snapshot exposes controller_logs and each worker's daemon_logs summary.
	for _, n := range c.nodes {
		if err := worker.HeartbeatOnce(t.Context(), n.eng(), n.cluster, c.id, nil); err != nil {
			t.Fatal(err)
		}
	}
	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	if snap.ControllerLogs == nil || snap.ControllerLogs.Warns_24H != 1 || snap.ControllerLogs.Errors_24H != 0 {
		t.Errorf("snapshot.controller_logs = %+v", snap.ControllerLogs)
	}
	seenWorkerSummary := 0
	for _, n := range snap.Nodes {
		if n.Hostname == "asahimbp" && n.DaemonLogs != nil && n.DaemonLogs.Errors_24H == 1 {
			seenWorkerSummary++
		}
		if n.Hostname == "mini" && n.DaemonLogs != nil && n.DaemonLogs.Warns_24H == 1 {
			seenWorkerSummary++
		}
	}
	if seenWorkerSummary != 2 {
		t.Errorf("expected both workers' daemon_logs summaries in snapshot, got %d (%+v)", seenWorkerSummary, snap.Nodes)
	}

	// When one worker is halted/unreachable, querying all still returns healthy
	// daemons and flags the unreachable worker in sources; querying that worker
	// directly fails hard with WORKER_UNREACHABLE.
	c.nodes[1].halt()
	partial := &pb.GetDaemonLogsResponse{}
	c.ok(partial, "daemon-logs")
	if len(partial.Entries) != 2 {
		t.Errorf("partial entries = %d, want 2 (controller + asahimbp)", len(partial.Entries))
	}
	foundUnreachable := false
	for _, src := range partial.Sources {
		if src.Source == "mini" && !src.Reachable && src.Error != "" {
			foundUnreachable = true
		}
	}
	if !foundUnreachable {
		t.Errorf("expected mini marked unreachable in sources: %+v", partial.Sources)
	}
	if _, e := c.fails("daemon-logs", "--source", "mini"); e.Code != api.CodeWorkerUnreachable {
		t.Errorf("--source halted worker = %v, want WORKER_UNREACHABLE", e)
	}
}
