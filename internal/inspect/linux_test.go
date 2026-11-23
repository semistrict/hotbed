//go:build linux

package inspect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestLinuxInspectSelfProcess(t *testing.T) {
	// Open a real TCP listener so /proc/net/tcp socket resolution is exercised end-to-end.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp4: %v", err)
	}
	defer ln.Close()
	wantAddr := ln.Addr().String()

	res, err := (Linux{}).Inspect(context.Background(), Target{
		CommandID: "self-test",
		UnitName:  "hotbed-self-test",
		MainPID:   os.Getpid(),
	}, &pb.InspectCommandRequest{Id: "self-test"})
	if err != nil {
		t.Fatalf("Linux.Inspect: %v", err)
	}
	if len(res.Procs) != 1 || int(res.Procs[0].Pid) != os.Getpid() {
		t.Fatalf("expected self PID %d in procs, got %+v", os.Getpid(), res.Procs)
	}
	if res.Procs[0].RssBytes <= 0 {
		t.Fatalf("expected positive RSS, got %d", res.Procs[0].RssBytes)
	}
	foundSock := false
	for _, f := range res.Files {
		if f.Kind == "socket" && f.SocketState == "LISTEN" && f.LocalAddr == wantAddr {
			foundSock = true
			break
		}
	}
	if !foundSock {
		t.Fatalf("expected listening TCP socket %s in open files, got %d files", wantAddr, len(res.Files))
	}
	if len(res.Maps) == 0 || res.Maps[0].RssBytes <= 0 {
		t.Fatalf("expected memory map for self process, got %+v", res.Maps)
	}
	if len(res.Limits) == 0 || len(res.Limits[0].Items) == 0 {
		t.Fatalf("expected rlimits for self process, got %+v", res.Limits)
	}
	if len(res.AvailableTools) == 0 {
		t.Fatalf("expected tool capabilities list")
	}
}

func TestLinuxCapabilitiesInBaseRootDir(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"strace", "perf", "eu-stack", "gdb", "bpftrace"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	l := Linux{BaseRootDir: func() string { return root }}
	caps := l.Capabilities()
	if len(caps) != 5 {
		t.Fatalf("got %d capabilities, want 5", len(caps))
	}
	for _, c := range caps {
		if !c.Available {
			t.Errorf("tool %s reported unavailable in fake rootfs: %s", c.Name, c.Detail)
		}
		if c.Detail != "/usr/bin/"+c.Name {
			t.Errorf("tool %s detail = %q, want /usr/bin/%s", c.Name, c.Detail, c.Name)
		}
	}

	// Empty rootfs reports all 5 unavailable with base-image hint (never leaking host PATH).
	empty := t.TempDir()
	res, err := Linux{BaseRootDir: func() string { return empty }}.Inspect(context.Background(), Target{
		CommandID: "empty-root",
		UnitName:  "hotbed-empty-root",
		MainPID:   os.Getpid(),
	}, &pb.InspectCommandRequest{Id: "empty-root", Sections: []pb.InspectSection{pb.InspectSection_INSPECT_SECTION_PROCS}})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	for _, c := range res.AvailableTools {
		if c.Available {
			t.Errorf("tool %s unexpectedly available in empty rootfs", c.Name)
		}
	}
}

func TestLinuxSampleSelfProcess(t *testing.T) {
	// Open a TCP listener and a pipe so socket & pipe peer correlation are exercised on real Linux /proc.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp4: %v", err)
	}
	defer ln.Close()
	wantAddr := ln.Addr().String()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	var chunks []*pb.SampleCommandChunk
	err = (Linux{}).Sample(context.Background(), Target{
		CommandID: "self-sample",
		UnitName:  "hotbed-self-sample",
		MainPID:   os.Getpid(),
	}, &pb.SampleCommandRequest{
		Id:              "self-sample",
		DurationSeconds: 1,
	}, func(ch *pb.SampleCommandChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("Linux.Sample: %v", err)
	}
	if len(chunks) < 2 || !chunks[len(chunks)-1].Done || chunks[len(chunks)-1].Sample == nil {
		t.Fatalf("expected progress + final sample chunk, got %d chunks", len(chunks))
	}
	s := chunks[len(chunks)-1].Sample
	if s.PollCount < 4 {
		t.Fatalf("expected at least 4 polls, got %d", s.PollCount)
	}
	if len(s.Procs) != 1 || int(s.Procs[0].Pid) != os.Getpid() || len(s.Procs[0].Threads) == 0 {
		t.Fatalf("expected self PID %d with threads, got %+v", os.Getpid(), s.Procs)
	}
	foundSock, foundPipePeer := false, false
	for _, f := range s.Files {
		if f.Kind == "socket" && f.SocketState == "LISTEN" && f.LocalAddr == wantAddr {
			foundSock = true
		}
		if f.Kind == "pipe" && f.Peer != "" {
			foundPipePeer = true
		}
	}
	if !foundSock {
		t.Fatalf("expected listening TCP socket %s in sampled files", wantAddr)
	}
	if !foundPipePeer {
		t.Fatalf("expected pipe peer correlation for self pipe in sampled files")
	}
}

func TestLinuxSampleStationaryChildNotPerturbedByUserStackCapture(t *testing.T) {
	// Spawn a sleeping child process and verify that sampling it reports 0
	// context switches and marks it stationary — ensuring user-space
	// unwinding (ptrace) runs strictly after T1 and never pollutes T0->T1 deltas.
	child := exec.Command("sleep", "15")
	if err := child.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	time.Sleep(150 * time.Millisecond)

	var final *pb.ProcessSample
	err := (Linux{}).Sample(context.Background(), Target{
		CommandID: "sleep-child",
		UnitName:  "hotbed-sleep-child",
		MainPID:   child.Process.Pid,
	}, &pb.SampleCommandRequest{
		Id:              "sleep-child",
		DurationSeconds: 1,
	}, func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			final = ch.Sample
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Linux.Sample(sleep): %v", err)
	}
	if final == nil || len(final.Procs) != 1 || len(final.Procs[0].Threads) == 0 {
		t.Fatalf("unexpected sample: %+v", final)
	}
	th := final.Procs[0].Threads[0]
	if th.VoluntaryCtxtDelta != 0 || !th.Stationary {
		t.Fatalf("expected sleeping child to have 0 voluntary ctx switches and stationary=true, got vol=%d stationary=%v polls=%d/%d",
			th.VoluntaryCtxtDelta, th.Stationary, th.PollsSameStack, th.TotalPolls)
	}
}

// A sandboxed command's processes are in cgroups below its unit's
// (board 8145): every one of them is the command's.
func TestCgroupPIDsWalksSubCgroups(t *testing.T) {
	unit := t.TempDir()
	for dir, procs := range map[string]string{
		"":                "",
		"sandbox":         "41\n",
		"command":         "7\n9\n",
		"command/payload": "12\n",
		"command/empty":   "",
	} {
		if err := os.MkdirAll(filepath.Join(unit, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unit, dir, "cgroup.procs"), []byte(procs), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pids, err := cgroupPIDs(unit)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{7, 9, 12, 41}; !slices.Equal(pids, want) {
		t.Errorf("cgroupPIDs = %v, want %v", pids, want)
	}
	if _, err := cgroupPIDs(filepath.Join(unit, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cgroup that is gone: err = %v, want not exist", err)
	}
}

func TestHasDStateThread(t *testing.T) {
	if !hasDStateThread(procSnap{procBase: procBase{stat: ProcStat{State: "D"}}}) {
		t.Fatal("expected leader D-state to return true")
	}
	if !hasDStateThread(procSnap{
		procBase: procBase{stat: ProcStat{State: "S"}},
		threads: map[int32]threadSnap{
			100: {tid: 100, state: "S"},
			101: {tid: 101, state: "D"},
		},
	}) {
		t.Fatal("expected worker thread D-state with sleeping leader to return true")
	}
	if hasDStateThread(procSnap{
		procBase: procBase{stat: ProcStat{State: "S"}},
		threads: map[int32]threadSnap{
			100: {tid: 100, state: "S"},
			101: {tid: 101, state: "R"},
		},
	}) {
		t.Fatal("expected all S/R threads to return false")
	}
}

func TestRunToolCommandKillsProcessGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := runToolCommand(ctx, "/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				childPID = p
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		cancel()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for child pid")
	}

	cancel()
	_ = cmd.Wait()

	// Wait briefly for kernel to reap the killed grandchild.
	killed := false
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			killed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !killed {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("grandchild PID %d survived process-group kill on cancel", childPID)
	}
}

func TestRunTimedToolAbortOnLineErrorDoesNotWaitDuration(t *testing.T) {
	start := time.Now()
	wantErr := errors.New("stream closed")
	// Process ignores SIGINT and sleeps 30s; runTimedTool must SIGKILL it promptly
	// after onLine fails instead of blocking for the full 30s duration.
	_, err := runTimedTool(context.Background(), 30*time.Second, "/bin/sh", []string{
		"-c", "trap '' INT; echo ready; sleep 30",
	}, func(line string) error {
		return wantErr
	})
	elapsed := time.Since(start)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runTimedTool took %v after onLine error, want < 5s", elapsed)
	}
}

func TestPerfRecordArgs(t *testing.T) {
	t.Run("cgroup target without explicit pid uses -a -e cpu-clock -G", func(t *testing.T) {
		got := perfRecordArgs(
			Target{CommandID: "cmd-1", Cgroup: "/hotbed.slice/hotbed-abc.service"},
			[]int{101, 102},
			&pb.TraceCommandRequest{DurationSeconds: 7, FrequencyHz: 199},
			"/tmp/perf.data",
		)
		want := []string{
			"record",
			"-F", "199",
			"-g", "--call-graph", "dwarf,16384",
			"-a", "-e", "cpu-clock", "-G", "hotbed.slice/hotbed-abc.service",
			"-o", "/tmp/perf.data",
			"--", "sleep", "7",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("perfRecordArgs(cgroup) =\n  %q\nwant:\n  %q", got, want)
		}
		eIdx := slices.Index(got, "-e")
		gIdx := slices.Index(got, "-G")
		if eIdx < 0 || eIdx+1 >= len(got) || got[eIdx+1] != "cpu-clock" || gIdx < 0 || eIdx >= gIdx {
			t.Fatalf("expected -e cpu-clock before -G in %q", got)
		}
	})

	t.Run("cgroup target without leading slash preserves relative cgroup path", func(t *testing.T) {
		got := perfRecordArgs(
			Target{CommandID: "cmd-1", Cgroup: "hotbed.slice/hotbed-abc.service"},
			[]int{101},
			&pb.TraceCommandRequest{},
			"/tmp/perf.data",
		)
		want := []string{
			"record",
			"-F", "99",
			"-g", "--call-graph", "dwarf,16384",
			"-a", "-e", "cpu-clock", "-G", "hotbed.slice/hotbed-abc.service",
			"-o", "/tmp/perf.data",
			"--", "sleep", "5",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("perfRecordArgs(rel cgroup) =\n  %q\nwant:\n  %q", got, want)
		}
	})

	t.Run("explicit pid overrides cgroup and uses -p", func(t *testing.T) {
		got := perfRecordArgs(
			Target{CommandID: "cmd-1", Cgroup: "/hotbed.slice/hotbed-abc.service"},
			[]int{4242},
			&pb.TraceCommandRequest{Pid: 4242, DurationSeconds: 3, FrequencyHz: 50},
			"/var/tmp/perf.data",
		)
		want := []string{
			"record",
			"-F", "50",
			"-g", "--call-graph", "dwarf,16384",
			"-p", "4242",
			"-o", "/var/tmp/perf.data",
			"--", "sleep", "3",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("perfRecordArgs(explicit pid) =\n  %q\nwant:\n  %q", got, want)
		}
	})

	t.Run("empty cgroup falls back to -p with pid CSV", func(t *testing.T) {
		got := perfRecordArgs(
			Target{CommandID: "cmd-1"},
			[]int{10, 20, 30},
			&pb.TraceCommandRequest{DurationSeconds: 2, FrequencyHz: 99},
			"/tmp/perf.data",
		)
		want := []string{
			"record",
			"-F", "99",
			"-g", "--call-graph", "dwarf,16384",
			"-p", "10,20,30",
			"-o", "/tmp/perf.data",
			"--", "sleep", "2",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("perfRecordArgs(no cgroup) =\n  %q\nwant:\n  %q", got, want)
		}
	})
}

func TestIsPerfNoSuchProcessAndFilterLivePIDs(t *testing.T) {
	exit244 := exec.Command("/bin/sh", "-c", "exit 244").Run()
	if !isPerfNoSuchProcess(exit244, "") {
		t.Fatalf("expected exit 244 to match isPerfNoSuchProcess")
	}
	exit1 := exec.Command("/bin/sh", "-c", "exit 1").Run()
	if !isPerfNoSuchProcess(exit1, "Error:\nCouldn't create thread/CPU maps: No such process") {
		t.Fatalf("expected 'No such process' stderr to match isPerfNoSuchProcess")
	}
	if isPerfNoSuchProcess(exit1, "Permission denied") {
		t.Fatalf("expected unrelated exit 1 not to match isPerfNoSuchProcess")
	}
	if isPerfNoSuchProcess(nil, "No such process") {
		t.Fatalf("expected nil error not to match isPerfNoSuchProcess")
	}

	dead := exec.Command("/bin/true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	live := filterLivePIDs([]int{os.Getpid(), dead.Process.Pid, 0, -1})
	if want := []int{os.Getpid()}; !slices.Equal(live, want) {
		t.Fatalf("filterLivePIDs = %v, want %v", live, want)
	}
}

func TestRunPerfCPURejectsUnsafeCgroup(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "perf"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	err := runPerfCPU(context.Background(), "", Target{
		CommandID: "cmd-unsafe",
		Cgroup:    "/hotbed.slice/../../etc/passwd",
		MainPID:   os.Getpid(),
	}, []int{os.Getpid()}, &pb.TraceCommandRequest{
		Kind:            pb.TraceKind_TRACE_KIND_PERF_CPU,
		DurationSeconds: 1,
	}, func(*pb.TraceCommandChunk) error { return nil })
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
		t.Fatalf("runPerfCPU(unsafe cgroup) err = %v, want INVALID_ARGS *api.Error", err)
	}
}

func TestRunPerfCPURetriesMultiPIDAfterTransientExit(t *testing.T) {
	dead := exec.Command("/bin/true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	livePID := os.Getpid()
	deadPID := dead.Process.Pid

	binDir := t.TempDir()
	logFile := filepath.Join(binDir, "perf-invocations.log")
	fakePerf := fmt.Sprintf(`#!/bin/sh
set -eu
echo "$*" >> %q
sub="$1"
shift
case "$sub" in
  record)
    out=""
    pids=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -o) out="$2"; shift 2 ;;
        -p) pids="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case ",$pids," in
      *,%d,*)
        echo "Error:" >&2
        echo "Couldn't create thread/CPU maps: No such process" >&2
        exit 244
        ;;
    esac
    printf 'fake-perf-data\n' > "$out"
    exit 0
    ;;
  script)
    printf 'worker %d 100.001: 1000000 cpu-clock:\n\t401000 main.work+0x10 (/usr/bin/worker)\n\n'
    exit 0
    ;;
  report)
    printf '100.00%%  worker  worker  [.] main.work\n'
    exit 0
    ;;
esac
exit 1
`, logFile, deadPID, livePID)
	if err := os.WriteFile(filepath.Join(binDir, "perf"), []byte(fakePerf), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	var final *pb.TraceSummary
	err := runPerfCPU(context.Background(), "", Target{
		CommandID: "cmd-retry",
		MainPID:   livePID,
	}, []int{livePID, deadPID}, &pb.TraceCommandRequest{
		Kind:            pb.TraceKind_TRACE_KIND_PERF_CPU,
		DurationSeconds: 1,
		FrequencyHz:     99,
	}, func(ch *pb.TraceCommandChunk) error {
		if ch.Done {
			final = ch.Summary
		}
		return nil
	})
	if err != nil {
		t.Fatalf("runPerfCPU retry failed: %v", err)
	}
	if final == nil {
		t.Fatal("expected final TraceSummary chunk")
	}
	if want := []int32{int32(livePID)}; !slices.Equal(final.PidsAttached, want) {
		t.Fatalf("PidsAttached = %v, want %v", final.PidsAttached, want)
	}
	if !strings.Contains(final.CommandLine, fmt.Sprintf("-p %d", livePID)) || strings.Contains(final.CommandLine, strconv.Itoa(deadPID)) {
		t.Fatalf("CommandLine = %q, want retry with only live PID %d", final.CommandLine, livePID)
	}
	invocations, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(invocations)), "\n"); len(lines) != 4 {
		t.Fatalf("expected 4 perf invocations (record fail, record retry, script, report), got %d:\n%s", len(lines), string(invocations))
	}
}

func TestLinuxPerfCPUTransientChildProcess(t *testing.T) {
	perfBin, err := exec.LookPath("perf")
	if err != nil {
		t.Skip("perf not installed")
	}
	// Read our own cgroup v2 path (format "0::/path/to/cgroup").
	var selfCgroup string
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if cg, ok := strings.CutPrefix(line, "0::"); ok && cg != "" && cg != "/" {
				if _, serr := os.Stat(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(cg, "/"))); serr == nil {
					selfCgroup = cg
					break
				}
			}
		}
	}

	probeArgs := []string{"record", "-o", "/dev/null", "--", "true"}
	if selfCgroup != "" {
		probeArgs = []string{"record", "-a", "-e", "cpu-clock", "-G", strings.TrimPrefix(selfCgroup, "/"), "-o", "/dev/null", "--", "true"}
	}
	probe := exec.Command(perfBin, probeArgs...)
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("perf record not permitted in this environment: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	// Spawn a parent that continually spawns short-lived children while perf attaches.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spawner := exec.CommandContext(ctx, "/bin/sh", "-c", "while true; do /bin/true & /bin/true & wait; done")
	if err := spawner.Start(); err != nil {
		t.Fatalf("start transient child spawner: %v", err)
	}
	defer func() {
		cancel()
		_ = spawner.Process.Kill()
		_ = spawner.Wait()
	}()

	var final *pb.TraceSummary
	target := Target{
		CommandID: "transient-perf",
		Cgroup:    selfCgroup,
		MainPID:   spawner.Process.Pid,
	}
	err = (Linux{}).Trace(context.Background(), target, &pb.TraceCommandRequest{
		Id:              "transient-perf",
		Kind:            pb.TraceKind_TRACE_KIND_PERF_CPU,
		DurationSeconds: 1,
		FrequencyHz:     49,
	}, func(ch *pb.TraceCommandChunk) error {
		if ch.Done {
			final = ch.Summary
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Linux.Trace(PERF_CPU) with transient children failed: %v", err)
	}
	if final == nil {
		t.Fatal("expected final TraceSummary chunk")
	}
	if selfCgroup != "" && !strings.Contains(final.CommandLine, "-a -e cpu-clock -G ") {
		t.Fatalf("expected cgroup perf command line with -a -e cpu-clock -G, got %q", final.CommandLine)
	}
}

func TestCollectBPFCgroupDirsAndPrograms(t *testing.T) {
	allKinds := []pb.TraceKind{
		pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
		pb.TraceKind_TRACE_KIND_BPF_OFFCPU,
		pb.TraceKind_TRACE_KIND_BPF_NET,
	}

	t.Run("unit dir with child sub-cgroups resolves all directories", func(t *testing.T) {
		unit := t.TempDir()
		for _, sub := range []string{"payload", "vm1"} {
			if err := os.MkdirAll(filepath.Join(unit, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs, err := collectBPFCgroupDirs(unit, maxBPFCgroups)
		if err != nil {
			t.Fatalf("collectBPFCgroupDirs: %v", err)
		}
		want := []string{
			unit,
			filepath.Join(unit, "payload"),
			filepath.Join(unit, "vm1"),
		}
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectBPFCgroupDirs = %v, want %v", dirs, want)
		}

		// Map into synthetic /sys/fs/cgroup paths and verify all 5 curated probes.
		cgPaths := make([]string, len(dirs))
		for i, d := range dirs {
			rel, err := filepath.Rel(unit, d)
			if err != nil {
				t.Fatal(err)
			}
			cgPaths[i] = filepath.Join("/sys/fs/cgroup/hotbed.slice/hotbed-abc.service", rel)
		}
		for _, kind := range allKinds {
			_, prog, err := buildBPFTraceProgramForCgroups(kind, cgPaths, 4, 0)
			if err != nil {
				t.Fatalf("buildBPFTraceProgramForCgroups(%v): %v", kind, err)
			}
			wantPred := `cgroup == cgroupid("/sys/fs/cgroup/hotbed.slice/hotbed-abc.service")`
			if !strings.Contains(prog, wantPred) {
				t.Errorf("%v missing unit cgroup predicate: %s", kind, prog)
			}
			if !strings.Contains(prog, "interval:s:4 { exit(); }") {
				t.Errorf("%v missing interval exit: %s", kind, prog)
			}

			_, progPID, err := buildBPFTraceProgramForCgroups(kind, cgPaths, 2, 1337)
			if err != nil {
				t.Fatalf("buildBPFTraceProgramForCgroups(%v, pid): %v", kind, err)
			}
			if kind == pb.TraceKind_TRACE_KIND_BPF_OFFCPU {
				if !strings.Contains(progPID, "&& (pid == 1337 || args.prev_pid == 1337)") {
					t.Errorf("offcpu missing pid predicate: %s", progPID)
				}
			} else if !strings.Contains(progPID, "&& pid == 1337") {
				t.Errorf("%v missing pid predicate: %s", kind, progPID)
			}
		}
	})

	t.Run("unit without child sub-cgroups resolves unit dir", func(t *testing.T) {
		unit := t.TempDir()
		dirs, err := collectBPFCgroupDirs(unit, maxBPFCgroups)
		if err != nil {
			t.Fatalf("collectBPFCgroupDirs: %v", err)
		}
		if want := []string{unit}; !slices.Equal(dirs, want) {
			t.Fatalf("collectBPFCgroupDirs = %v, want %v", dirs, want)
		}
	})

	t.Run("missing unit dir returns ErrNotExist", func(t *testing.T) {
		unit := t.TempDir()
		if _, err := collectBPFCgroupDirs(filepath.Join(unit, "gone"), maxBPFCgroups); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("gone unit err = %v, want ErrNotExist", err)
		}
	})

	t.Run("exact cap succeeds while exceeding cap returns actionable error", func(t *testing.T) {
		fsRoot := t.TempDir()
		cgRel := "hotbed.slice/hotbed-cap.service"
		cgPath := "/sys/fs/cgroup/" + cgRel
		unit := filepath.Join(fsRoot, cgRel)
		// Create unit + 31 sub-cgroups = 32 total (exact limit).
		if err := os.MkdirAll(unit, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < maxBPFCgroups; i++ {
			if err := os.MkdirAll(filepath.Join(unit, fmt.Sprintf("sub-%02d", i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs, err := resolveBPFCgroupPathsAt(fsRoot, cgPath, 0)
		if err != nil {
			t.Fatalf("resolveBPFCgroupPathsAt(exact limit %d) err = %v", maxBPFCgroups, err)
		}
		if len(dirs) != maxBPFCgroups {
			t.Fatalf("len(dirs) = %d, want %d", len(dirs), maxBPFCgroups)
		}

		// Adding one more sub-cgroup exceeds maxBPFCgroups (33 > 32) and must fail instead of silently omitting.
		if err := os.MkdirAll(filepath.Join(unit, "sub-32"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err = resolveBPFCgroupPathsAt(fsRoot, cgPath, 0)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
			t.Fatalf("resolveBPFCgroupPathsAt(>32 sub-cgroups) err = %v, want CodeInvalidArgs *api.Error", err)
		}
		if !strings.Contains(err.Error(), "33") || !strings.Contains(err.Error(), fmt.Sprintf("limit %d", maxBPFCgroups)) || !strings.Contains(err.Error(), "--pid") {
			t.Fatalf("err = %q, want count 33, limit %d, and --pid guidance", err.Error(), maxBPFCgroups)
		}
	})

	t.Run("invalid sub-cgroup directory name returns error instead of silently omitting", func(t *testing.T) {
		for _, badName := range []string{"bad dir", `bad"quote`, "bad;cmd"} {
			t.Run(badName, func(t *testing.T) {
				fsRoot := t.TempDir()
				cgRel := "hotbed.slice/hotbed-badname.service"
				cgPath := "/sys/fs/cgroup/" + cgRel
				unit := filepath.Join(fsRoot, cgRel)
				for _, sub := range []string{"ok", badName} {
					if err := os.MkdirAll(filepath.Join(unit, sub), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				_, err := resolveBPFCgroupPathsAt(fsRoot, cgPath, 0)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
					t.Fatalf("resolveBPFCgroupPathsAt(%q) err = %v, want CodeInvalidArgs *api.Error", badName, err)
				}
				if !strings.Contains(err.Error(), "--pid") {
					t.Fatalf("err = %q, want --pid guidance", err.Error())
				}
			})
		}
	})
}

func TestParseCgroupV2Path(t *testing.T) {
	got, ok := parseCgroupV2Path("12:memory:/legacy\n0::/user.slice/hotbed-abc.service/command\n")
	if !ok || got != "/sys/fs/cgroup/user.slice/hotbed-abc.service/command" {
		t.Fatalf("parseCgroupV2Path = (%q, %v), want (/sys/fs/cgroup/user.slice/hotbed-abc.service/command, true)", got, ok)
	}
	if got, ok := parseCgroupV2Path("0::/\n"); ok {
		t.Fatalf("expected root cgroup to return false, got %q", got)
	}
	if got, ok := parseCgroupV2Path(""); ok {
		t.Fatalf("expected empty content to return false, got %q", got)
	}
}

func TestRunTimedToolWithDeadlineReset(t *testing.T) {
	start := time.Now()
	// Simulate bpftrace v0.25 printing "Attached 2 probes" and hanging because
	// interval:s:N never fires; resetting the timer on "Attached " must SIGINT
	// it promptly rather than waiting for the 30s outer timeout.
	raw, timedOut, err := runTimedToolWithDeadline(context.Background(), 30*time.Second, "/bin/sh", []string{
		"-c", `trap 'echo "@[vfs_read, sh]: 42"; exit 0' INT; echo "Attached 2 probes"; sleep 30`,
	}, func(line string, resetTimer func(time.Duration)) error {
		if strings.HasPrefix(line, "Attached ") {
			resetTimer(150 * time.Millisecond)
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runTimedToolWithDeadline err = %v", err)
	}
	if !timedOut {
		t.Fatalf("expected timedOut=true after deadline reset")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v after deadline reset, want < 5s", elapsed)
	}
	if !strings.Contains(raw, "@[vfs_read, sh]: 42") {
		t.Fatalf("expected SIGINT-flushed map output, got %q", raw)
	}
}

func TestRunBPFTraceArgsAndErrorPropagation(t *testing.T) {
	// Locate our own real cgroup v2 directory so runBPFTrace's cgroup checks pass.
	selfCG, ok := readPIDCgroupPath(os.Getpid())
	if !ok {
		t.Skip("no cgroup v2 path in /proc/self/cgroup")
	}
	if _, err := os.Stat(selfCG); err != nil {
		t.Skipf("self cgroup %s not accessible: %v", selfCG, err)
	}
	relCG := strings.TrimPrefix(selfCG, "/sys/fs/cgroup")

	t.Run("passes -B none and tightens timeout on Attached banner", func(t *testing.T) {
		binDir := t.TempDir()
		argLog := filepath.Join(binDir, "args.log")
		fakeBPF := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
trap 'echo "@[kprobe:vfs_read, sh]: 7"; exit 0' INT
echo "Attached 5 probes"
sleep 30
`, argLog)
		if err := os.WriteFile(filepath.Join(binDir, "bpftrace"), []byte(fakeBPF), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

		start := time.Now()
		var final *pb.TraceSummary
		err := runBPFTrace(context.Background(), "", Target{
			CommandID: "fake-bpf",
			Cgroup:    relCG,
			MainPID:   os.Getpid(),
		}, []int{os.Getpid()}, &pb.TraceCommandRequest{
			Id:              "fake-bpf",
			Kind:            pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
			DurationSeconds: 1,
		}, func(ch *pb.TraceCommandChunk) error {
			if ch.Done {
				final = ch.Summary
			}
			return nil
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("runBPFTrace: %v", err)
		}
		if elapsed > 6*time.Second {
			t.Fatalf("runBPFTrace took %v (expected tightened deadline ~3s, not 11s outer timeout)", elapsed)
		}
		if final == nil || !strings.Contains(final.Raw, "@[kprobe:vfs_read, sh]: 7") {
			t.Fatalf("unexpected summary: %+v", final)
		}
		argsBytes, err := os.ReadFile(argLog)
		if err != nil {
			t.Fatal(err)
		}
		gotArgs := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
		if len(gotArgs) != 4 || gotArgs[0] != "-B" || gotArgs[1] != "none" || gotArgs[2] != "-e" {
			t.Fatalf("bpftrace argv = %q, want [-B none -e <prog>]", gotArgs)
		}
		if !strings.Contains(gotArgs[3], fmt.Sprintf(`cgroup == cgroupid(%q)`, selfCG)) {
			t.Fatalf("program missing resolved cgroup %q: %s", selfCG, gotArgs[3])
		}
	})

	t.Run("non-zero bpftrace exit returns error even when stderr is non-empty", func(t *testing.T) {
		binDir := t.TempDir()
		fakeBPF := "#!/bin/sh\necho 'ERROR: Failed to get cgroup id' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(binDir, "bpftrace"), []byte(fakeBPF), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

		err := runBPFTrace(context.Background(), "", Target{
			CommandID: "fake-bpf-err",
			Cgroup:    relCG,
			MainPID:   os.Getpid(),
		}, []int{os.Getpid()}, &pb.TraceCommandRequest{
			Id:              "fake-bpf-err",
			Kind:            pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
			DurationSeconds: 1,
		}, func(*pb.TraceCommandChunk) error { return nil })
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInternal || !strings.Contains(err.Error(), "Failed to get cgroup id") {
			t.Fatalf("err = %v, want CodeInternal containing bpftrace stderr", err)
		}
	})

	t.Run("startup timeout before Attached banner returns error even on exit 130", func(t *testing.T) {
		origHeadroom := bpfStartupHeadroom
		bpfStartupHeadroom = 200 * time.Millisecond
		t.Cleanup(func() { bpfStartupHeadroom = origHeadroom })

		binDir := t.TempDir()
		fakeBPF := "#!/bin/sh\ntrap 'exit 130' INT\nsleep 30\n"
		if err := os.WriteFile(filepath.Join(binDir, "bpftrace"), []byte(fakeBPF), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

		var doneSeen bool
		err := runBPFTrace(context.Background(), "", Target{
			CommandID: "fake-bpf-timeout",
			Cgroup:    relCG,
			MainPID:   os.Getpid(),
		}, []int{os.Getpid()}, &pb.TraceCommandRequest{
			Id:              "fake-bpf-timeout",
			Kind:            pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
			DurationSeconds: 1,
		}, func(ch *pb.TraceCommandChunk) error {
			if ch.Done {
				doneSeen = true
			}
			return nil
		})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInternal || !strings.Contains(err.Error(), "timed out before attaching probes") {
			t.Fatalf("err = %v, want CodeInternal startup-timeout error", err)
		}
		if doneSeen {
			t.Fatalf("expected no Done chunk on startup timeout")
		}
	})

	t.Run("startup timeout with partial output before Attached banner returns error", func(t *testing.T) {
		origHeadroom := bpfStartupHeadroom
		bpfStartupHeadroom = 200 * time.Millisecond
		t.Cleanup(func() { bpfStartupHeadroom = origHeadroom })

		binDir := t.TempDir()
		fakeBPF := "#!/bin/sh\ntrap 'exit 130' INT\necho 'WARNING: compiling probes'\nsleep 30\n"
		if err := os.WriteFile(filepath.Join(binDir, "bpftrace"), []byte(fakeBPF), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

		err := runBPFTrace(context.Background(), "", Target{
			CommandID: "fake-bpf-timeout-partial",
			Cgroup:    relCG,
			MainPID:   os.Getpid(),
		}, []int{os.Getpid()}, &pb.TraceCommandRequest{
			Id:              "fake-bpf-timeout-partial",
			Kind:            pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
			DurationSeconds: 1,
		}, func(*pb.TraceCommandChunk) error { return nil })
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInternal ||
			!strings.Contains(err.Error(), "timed out before attaching probes") ||
			!strings.Contains(err.Error(), "WARNING: compiling probes") {
			t.Fatalf("err = %v, want CodeInternal startup-timeout error with partial output", err)
		}
	})

	t.Run("clean exit with empty output before Attached banner returns error", func(t *testing.T) {
		binDir := t.TempDir()
		fakeBPF := "#!/bin/sh\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, "bpftrace"), []byte(fakeBPF), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

		err := runBPFTrace(context.Background(), "", Target{
			CommandID: "fake-bpf-empty",
			Cgroup:    relCG,
			MainPID:   os.Getpid(),
		}, []int{os.Getpid()}, &pb.TraceCommandRequest{
			Id:              "fake-bpf-empty",
			Kind:            pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
			DurationSeconds: 1,
		}, func(*pb.TraceCommandChunk) error { return nil })
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInternal || !strings.Contains(err.Error(), "before attaching probes") {
			t.Fatalf("err = %v, want CodeInternal before-attaching error", err)
		}
	})
}

func isBPFTracePermissionError(out string) bool {
	lower := strings.ToLower(out)
	for _, needle := range []string{
		"operation not permitted",
		"permission denied",
		"eperm",
		"eacces",
		"only supports running as the root user",
		"must be run as root",
		"cap_bpf",
		"cap_perfmon",
		"cap_sys_admin",
		"lockdown",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func TestLinuxBPFTraceLiveProbes(t *testing.T) {
	bpfBin, err := exec.LookPath("bpftrace")
	if err != nil {
		t.Skip("bpftrace not installed")
	}
	selfCG, ok := readPIDCgroupPath(os.Getpid())
	if !ok {
		t.Skip("no cgroup v2 path in /proc/self/cgroup")
	}
	if _, err := os.Stat(selfCG); err != nil {
		t.Skipf("self cgroup %s not accessible: %v", selfCG, err)
	}
	probeCtx, probeCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer probeCancel()
	probe := runToolCommand(probeCtx, bpfBin, "-B", "none", "-e", "interval:ms:100 { exit(); }")
	if out, err := probe.CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		if probeCtx.Err() != nil {
			t.Fatalf("bpftrace capability probe timed out (%v): %s", probeCtx.Err(), outStr)
		}
		if isBPFTracePermissionError(outStr) {
			t.Skipf("bpftrace not permitted in this environment: %v (%s)", err, outStr)
		}
		t.Fatalf("bpftrace capability probe failed: %v (%s)", err, outStr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := exec.CommandContext(ctx, "/bin/sh", "-c", "while true; do cat /etc/hostname >/dev/null; sleep 0.05; done")
	if err := worker.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer func() {
		cancel()
		_ = worker.Process.Kill()
		_ = worker.Wait()
	}()

	relCG := strings.TrimPrefix(selfCG, "/sys/fs/cgroup")
	target := Target{
		CommandID: "live-bpf",
		Cgroup:    relCG,
		MainPID:   worker.Process.Pid,
	}

	for _, tc := range []struct {
		name       string
		kind       pb.TraceKind
		wantSubstr string
	}{
		{"opensnoop", pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP, "/etc/hostname"},
		{"vfsstat", pb.TraceKind_TRACE_KIND_BPF_VFSSTAT, "vfs_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			var final *pb.TraceSummary
			var streamed strings.Builder
			err := (Linux{}).Trace(context.Background(), target, &pb.TraceCommandRequest{
				Id:              "live-bpf",
				Kind:            tc.kind,
				DurationSeconds: 2,
			}, func(ch *pb.TraceCommandChunk) error {
				if ch.Text != "" {
					streamed.WriteString(ch.Text)
				}
				if ch.Done {
					final = ch.Summary
				}
				return nil
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Linux.Trace(%s): %v", tc.name, err)
			}
			if final == nil {
				t.Fatalf("expected final TraceSummary for %s", tc.name)
			}
			if !strings.Contains(final.Raw, tc.wantSubstr) {
				t.Fatalf("%s output missing %q (elapsed %v):\n%s", tc.name, tc.wantSubstr, elapsed, final.Raw)
			}
			if tc.kind == pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP && !strings.Contains(streamed.String(), tc.wantSubstr) {
				t.Fatalf("opensnoop did not stream %q live:\n%s", tc.wantSubstr, streamed.String())
			}
		})
	}
}
