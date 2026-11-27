package guestd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

func TestReadCgroupStatsCapturesOOMAndPeaks(t *testing.T) {
	cg := t.TempDir()
	files := map[string]string{
		"memory.stat":         "anon 4096\nfile 8192\n",
		"memory.swap.current": "1048576\n",
		"memory.peak":         "8388608000\n",
		"memory.swap.peak":    "536870912\n",
		"cpu.stat":            "usage_usec 420000\nuser_usec 300000\n",
		"memory.events":       "low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\noom_group_kill 0\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cg, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st := readCgroupStats(cg)
	if st.memAnon != 4096 {
		t.Fatalf("memAnon = %d, want 4096", st.memAnon)
	}
	if st.memSwap != 1048576 {
		t.Fatalf("memSwap = %d, want 1048576", st.memSwap)
	}
	if st.memPeak != 8388608000 {
		t.Fatalf("memPeak = %d, want 8388608000", st.memPeak)
	}
	if st.swapPeak != 536870912 {
		t.Fatalf("swapPeak = %d, want 536870912", st.swapPeak)
	}
	if st.cpuUsec != 420000 {
		t.Fatalf("cpuUsec = %d, want 420000", st.cpuUsec)
	}
	if !st.oomKilled {
		t.Fatal("expected oomKilled = true")
	}
}

func TestClassifyExitResult(t *testing.T) {
	if got := classifyExitResult(137, 9, true); got != "oom-kill" {
		t.Fatalf("oom + sig9 = %q, want oom-kill", got)
	}
	if got := classifyExitResult(137, 0, true); got != "oom-kill" {
		t.Fatalf("child oom + exit 137 = %q, want oom-kill", got)
	}
	if got := classifyExitResult(143, 15, false); got != "signal" {
		t.Fatalf("sig15 = %q, want signal", got)
	}
	if got := classifyExitResult(1, 0, false); got != "exit-code" {
		t.Fatalf("exit 1 = %q, want exit-code", got)
	}
	if got := classifyExitResult(0, 0, false); got != "success" {
		t.Fatalf("exit 0 = %q, want success", got)
	}
}

func TestWaitCommandCapturesSignalOOMAndPeakStatsBeforeCgroupRemoval(t *testing.T) {
	srv, err := NewServer(ServerConfig{WorkDir: t.TempDir(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a command killed by SIGKILL whose cgroup recorded an OOM kill
	// and peak memory before exit.
	cgDir := t.TempDir()
	for name, body := range map[string]string{
		"memory.stat":         "anon 0\n",
		"memory.swap.current": "0\n",
		"memory.peak":         "7516192768\n", // 7 GiB
		"memory.swap.peak":    "1073741824\n", // 1 GiB
		"cpu.stat":            "usage_usec 987654\n",
		"memory.events":       "oom 1\noom_kill 1\n",
	} {
		if err := os.WriteFile(filepath.Join(cgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	logPath := filepath.Join(srv.logDir, "oom-test.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	mc := &managedCmd{
		id:      "oom-test",
		group:   unit.NewProcGroup(cmd),
		cg:      &cmdCgroup{path: cgDir},
		logPath: logPath,
		logFile: logFile,
		state:   guestdv1.CommandState_COMMAND_STATE_RUNNING,
		mainPID: cmd.Process.Pid,
		started: time.Now().UnixMilli(),
		done:    make(chan struct{}),
	}
	go srv.waitCommand(mc)

	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	select {
	case <-mc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SIGKILLed command")
	}

	st := mc.snapshot()
	if st.State != guestdv1.CommandState_COMMAND_STATE_EXITED {
		t.Fatalf("state = %v, want EXITED", st.State)
	}
	if st.Signal != int32(syscall.SIGKILL) {
		t.Fatalf("signal = %d, want %d", st.Signal, syscall.SIGKILL)
	}
	if st.ExitCode != 137 {
		t.Fatalf("exit_code = %d, want 137", st.ExitCode)
	}
	if !st.OomKilled || st.Result != "oom-kill" {
		t.Fatalf("oom telemetry = (oom=%v result=%q), want (true, oom-kill)", st.OomKilled, st.Result)
	}
	if st.MemoryPeak != 7<<30 {
		t.Fatalf("memory_peak = %d, want %d", st.MemoryPeak, int64(7<<30))
	}
	if st.MemorySwapPeak != 1<<30 {
		t.Fatalf("memory_swap_peak = %d, want %d", st.MemorySwapPeak, int64(1<<30))
	}
	if st.CpuUsec != 987654 {
		t.Fatalf("cpu_usec = %d, want 987654", st.CpuUsec)
	}

	// Also verify a plain non-OOM signal kill via StartCommand/Status.
	startResp, err := srv.StartCommand(context.Background(), &guestdv1.StartCommandRequest{
		Id:   "sigterm-test",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	_ = syscall.Kill(int(startResp.MainPid), syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := srv.Status(context.Background(), &guestdv1.StatusRequest{Ids: []string{"sigterm-test"}})
		if err != nil {
			t.Fatal(err)
		}
		got := resp.Commands["sigterm-test"]
		if got.State == guestdv1.CommandState_COMMAND_STATE_EXITED {
			if got.Signal != int32(syscall.SIGTERM) || got.Result != "signal" || got.OomKilled {
				t.Fatalf("sigterm status = %+v, want signal=15 result=signal oom=false", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for sigterm-test to exit")
}
