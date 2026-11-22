//go:build linux && systemdtest

// Run as root on a systemd machine:
//
//	sudo HOTBED_TEST_USER=$USER HOTBED_TEST_DISK=/mnt/fxdax-scratch go test -tags systemdtest ./internal/unit/
package unit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testUser(t *testing.T) string {
	u := os.Getenv("HOTBED_TEST_USER")
	if u == "" {
		t.Fatal("set HOTBED_TEST_USER to the user commands run as")
	}
	return u
}

// waitExit waits for the unit's command to exit and returns its state.
func waitExit(t *testing.T, name string) State {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, DefaultSlice, name); err != nil {
		t.Fatalf("wait for %s: %v", name, err)
	}
	s, err := Status(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exited {
		t.Fatalf("%s has no processes left but is %+v", name, s)
	}
	return s
}

func name(t *testing.T) string {
	n := fmt.Sprintf("hotbed-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { Stop(context.WithoutCancel(t.Context()), n) })
	return n
}

func TestExitCodeAndLog(t *testing.T) {
	dir := t.TempDir()
	n := name(t)
	log := filepath.Join(dir, "out.log")
	err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, User: testUser(t), Log: log, CPUWeight: 50, MemoryLow: 64 << 20, MemoryHigh: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := waitExit(t, n)
	if s.ExitCode != 3 || s.Signal != 0 {
		t.Errorf("state %+v, want exit 3", s)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "out\nerr\n" {
		t.Errorf("log %q, want both streams", b)
	}
}

// workspace makes a workspace holding a marker file, and a mountpoint of its
// own to bind it at. Never the worker's /work: tidying that away on a live
// worker would take every running command's workspace (board 6114). Call
// before name, so the unit is stopped before the mountpoint goes.
func workspace(t *testing.T) (ws, target string) {
	ws = t.TempDir()
	if err := os.Chmod(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "marker"), []byte("forked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := fmt.Sprintf("/run/hotbed-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { os.RemoveAll(root) })
	return ws, root + "/work"
}

func TestBindShowsWorkspaceAtFixedPath(t *testing.T) {
	ws, target := workspace(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", "pwd; cat marker; whoami"}, User: testUser(t), Dir: target, Binds: []Bind{{Source: ws, Target: target}}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Fatalf("state %+v", s)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := target + "\nforked\n" + testUser(t) + "\n"; string(b) != want {
		t.Errorf("log %q, want %q", b, want)
	}
	// On the host, the mountpoint holds only its pin.
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 1 || entries[0].Name() != PinFile {
		t.Errorf("%s outside the command's namespace: %v %v, want only %s", target, entries, err, PinFile)
	}
}

// Removing a directory that is a mountpoint in another namespace detaches
// the mounts on it there: a running command would keep its working
// directory but lose its workspace's path. The host cannot remove a pinned
// mountpoint, so a command reading its workspace by absolute path goes on.
func TestRunningCommandKeepsItsWorkspaceWhenTheHostTidiesUp(t *testing.T) {
	ws, target := workspace(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", "sleep 1; cat " + target + "/marker"}, User: testUser(t), Dir: target, Binds: []Bind{{Source: ws, Target: target}}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Status(t.Context(), n); err != nil || !s.Running {
		t.Fatalf("after start: %+v %v", s, err)
	}
	if err := os.Remove(target); err == nil {
		t.Errorf("removed %s under a running command", target)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Errorf("state %+v, want exit 0", s)
	}
	if b, _ := os.ReadFile(log); string(b) != "forked\n" {
		t.Errorf("log %q, want the marker read by its absolute path", b)
	}
}

func TestFreezeThawStop(t *testing.T) {
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"sleep", "300"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	s, err := Status(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running || s.PID == 0 || !strings.HasSuffix(s.Cgroup, "/"+n+".service") {
		t.Fatalf("after start: %+v", s)
	}
	if err := Freeze(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); !s.Frozen {
		t.Errorf("after freeze: %+v", s)
	}
	if err := Reclaim(s.Cgroup, 1<<20); err != nil {
		t.Errorf("reclaim: %v", err)
	}
	if _, err := Swap(s.Cgroup); err != nil {
		t.Errorf("swap: %v", err)
	}
	if err := Thaw(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); s.Frozen || !s.Running {
		t.Errorf("after thaw: %+v", s)
	}
	if err := Stop(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); s.Loaded {
		t.Errorf("after stop: %+v", s)
	}
}

func TestWaitReturnsWhenTheUnitExits(t *testing.T) {
	n := name(t)
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"sleep", "1"}, User: testUser(t), Log: filepath.Join(t.TempDir(), "out.log")}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := Wait(short, DefaultSlice, n); err != context.DeadlineExceeded {
		t.Fatalf("wait on a running unit = %v, want the deadline", err)
	}
	start := time.Now()
	waitExit(t, n)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("wait returned %v after a 1s command that had run for 0.2s", d)
	}
	if err := Wait(t.Context(), DefaultSlice, "hotbed-test-no-such-unit"); err != nil {
		t.Errorf("wait on a unit that is not there = %v, want nil", err)
	}
}

// The command must run inside its unit's cgroup, where its limits, freezing
// and reclaim apply; a PAM login session would move it to user.slice.
func TestCommandRunsInItsUnitsCgroup(t *testing.T) {
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", `cat /proc/self/cgroup; echo "$USER $HOME"`}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	u := testUser(t)
	want := fmt.Sprintf("0::%s/%s.service\n%s /home/%s\n", SliceDir(DefaultSlice), n, u, u)
	if string(b) != want {
		t.Errorf("command saw %q, want %q", b, want)
	}
}

// A command's $variables are its shell's, not systemd's: systemd would
// substitute them in the command line before the shell runs.
func TestShellVariablesReachTheShell(t *testing.T) {
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", `x=shell; echo "[$x] [${x}]"`}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[shell] [shell]\n" {
		t.Errorf("command printed %q", b)
	}
}

// A worker may put its commands in a slice under another, such as
// user.slice, to share that slice's memory limit; Wait finds them there.
func TestNestedSlice(t *testing.T) {
	n := name(t)
	const slice = "user-hotbedtest.slice"
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Slice: slice, Argv: []string{"sh", "-c", "sleep 0.3; cat /proc/self/cgroup"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, slice, n); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0::/user.slice/user-hotbedtest.slice/" + n + ".service\n"; string(b) != want {
		t.Errorf("command saw %q, want %q", b, want)
	}
}

// systemd rewrites a transient unit from its file on daemon-reload: a unit
// running a script of many lines comes through it whole.
func TestUnitOutlivesADaemonReload(t *testing.T) {
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"bash", "-c", "sleep 0.5\necho after"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v: %s", err, out)
	}
	desc, err := exec.Command("systemctl", "show", "-P", "Description", n+".service").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(desc)); got != "hotbed bash -c sleep 0.5 echo after" {
		t.Errorf("description after the reload = %q", got)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Errorf("state %+v, want exit 0", s)
	}
	if b, _ := os.ReadFile(log); string(b) != "after\n" {
		t.Errorf("log %q", b)
	}
}

// A measurement's unit never swaps: memory.swap.max 0.
func TestNoSwapUnit(t *testing.T) {
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", `cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/memory.swap.max"`}, User: testUser(t), Log: log, NoSwap: true}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	if b, _ := os.ReadFile(log); string(b) != "0\n" {
		t.Errorf("a NoSwap unit's memory.swap.max = %q, want 0", b)
	}
}

// StartSlice starts a slice that has no unit file, and SetProperties sets,
// then resets, its properties.
func TestStartSliceAndSetProperties(t *testing.T) {
	const slice = "hotbedtest-props.slice"
	t.Cleanup(func() {
		// Stopped, and what set-property left of it removed: the host as it was.
		for _, args := range [][]string{{"stop", slice, "hotbedtest.slice"}, {"revert", slice}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				t.Errorf("systemctl %v: %v: %s", args, err, out)
			}
		}
	})
	if err := StartSlice(t.Context(), slice); err != nil {
		t.Fatal(err)
	}
	dir := "/sys/fs/cgroup" + SliceDir(slice)
	disk := os.Getenv("HOTBED_TEST_DISK")
	if disk == "" {
		t.Fatal("set HOTBED_TEST_DISK to a path on a block device, such as a scratch disk's mount")
	}
	if err := SetProperties(t.Context(), slice, "AllowedCPUs=0", "IOWriteBandwidthMax="+disk+" 32M"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dir + "/cpuset.cpus"); string(b) != "0\n" {
		t.Errorf("cpuset.cpus = %q, want 0", b)
	}
	// systemd's M is 1000², a disk's.
	if b, _ := os.ReadFile(dir + "/io.max"); !strings.Contains(string(b), " wbps=32000000 ") {
		t.Errorf("io.max = %q, want 32 MB/s of writes", b)
	}
	if err := SetProperties(t.Context(), slice, "AllowedCPUs=", "IOWriteBandwidthMax="); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dir + "/cpuset.cpus"); strings.TrimSpace(string(b)) != "" {
		t.Errorf("cpuset.cpus after the reset = %q, want none", b)
	}
	if b, _ := os.ReadFile(dir + "/io.max"); strings.Contains(string(b), "wbps=32000000") {
		t.Errorf("io.max after the reset = %q, want no cap", b)
	}
}
