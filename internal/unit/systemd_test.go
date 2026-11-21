//go:build linux && systemdtest

// Run as root on a systemd machine:
//
//	sudo HOTBED_TEST_USER=$USER go test -tags systemdtest ./internal/unit/
package unit

import (
	"context"
	"fmt"
	"os"
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

func TestBindShowsWorkspaceAtFixedPath(t *testing.T) {
	ws := t.TempDir()
	if err := os.Chmod(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "marker"), []byte("forked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	err := Start(t.Context(), Spec{Name: n, Argv: []string{"sh", "-c", "pwd; cat marker; whoami"}, User: testUser(t), Dir: "/work/app", Binds: []Bind{{Source: ws, Target: "/work/app"}}, Log: log})
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
	if want := "/work/app\nforked\n" + testUser(t) + "\n"; string(b) != want {
		t.Errorf("log %q, want %q", b, want)
	}
	// systemd leaves an empty mountpoint behind on the host.
	if entries, err := os.ReadDir("/work/app"); err != nil || len(entries) != 0 {
		t.Errorf("/work/app outside the command's namespace: %v %v, want an empty directory", entries, err)
	}
	os.Remove("/work/app")
	os.Remove("/work")
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
