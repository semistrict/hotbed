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

// waitExit polls until the unit's command exits. Units run asynchronously;
// there is no event to wait on without D-Bus.
func waitExit(t *testing.T, name string) State {
	t.Helper()
	for range 200 {
		s, err := Status(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Exited {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not exit in 10s", name)
	return State{}
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
