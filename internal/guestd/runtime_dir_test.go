package guestd_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/unit"
)

func startTestGuestdWithRunDir(t *testing.T, workDir, runDir string) *guestd.Client {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "gd-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  workDir,
		StateDir: t.TempDir(),
		RunDir:   runDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	paths := map[uint32]string{}
	listen := func(port uint32) net.Listener {
		p := filepath.Join(sockDir, fmt.Sprintf("p%d.sock", port))
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		paths[port] = p
		t.Cleanup(func() { _ = ln.Close() })
		return ln
	}

	ctrlLn := listen(guestd.ControlPort)
	shellLn := listen(guestd.ShellPort)

	go func() { _ = srv.ServeControl(ctx, ctrlLn) }()
	go func() { _ = srv.ServeShell(ctx, shellLn) }()

	client := guestd.NewClient(guestd.UnixMapDialer(paths))
	if _, err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return client
}

func TestGuestdCreatesRuntimeDirAndExportsXDGRuntimeDir(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	client := startTestGuestdWithRunDir(t, workDir, runDir)
	ctx := context.Background()

	uid := os.Getuid()
	gid := os.Getgid()
	if uid == 0 {
		uid, gid = 1000, 1000
	}
	wantDir := filepath.Join(runDir, "user", fmt.Sprintf("%d", uid))

	// 1. Command without XDG_RUNTIME_DIR in req.Env gets <runDir>/user/<uid>
	//    created (0700) and exported in XDG_RUNTIME_DIR.
	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-xdg-default",
		Argv: []string{"/bin/sh", "-c", `printf '%s' "$XDG_RUNTIME_DIR"`},
		Dir:  workDir,
		Env:  []string{"PATH=/usr/bin:/bin"},
		Uid:  int32(uid),
		Gid:  int32(gid),
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	var out bytes.Buffer
	if _, err := client.StreamLogs(ctx, "cmd-xdg-default", 0, true, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if strings.TrimSpace(out.String()) != wantDir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", out.String(), wantDir)
	}
	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat %s: %v", wantDir, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s perm = %04o, want 0700", wantDir, got)
	}

	// 2. Explicit XDG_RUNTIME_DIR in req.Env is preserved.
	if _, err := client.StartCommand(ctx, &guestdv1.StartCommandRequest{
		Id:   "cmd-xdg-override",
		Argv: []string{"/bin/sh", "-c", `printf '%s' "$XDG_RUNTIME_DIR"`},
		Dir:  workDir,
		Env:  []string{"PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=/custom/run"},
		Uid:  int32(uid),
		Gid:  int32(gid),
	}); err != nil {
		t.Fatalf("StartCommand override: %v", err)
	}
	out.Reset()
	if _, err := client.StreamLogs(ctx, "cmd-xdg-override", 0, true, &out); err != nil {
		t.Fatalf("StreamLogs override: %v", err)
	}
	if strings.TrimSpace(out.String()) != "/custom/run" {
		t.Fatalf("override XDG_RUNTIME_DIR = %q, want /custom/run", out.String())
	}

	// 3. Shell without XDG_RUNTIME_DIR also gets <runDir>/user/<uid> created
	//    and exported.
	if err := os.RemoveAll(wantDir); err != nil {
		t.Fatal(err)
	}
	var shOut, shErr bytes.Buffer
	h, err := client.StartShell(ctx, guestd.ShellOpenRequest{
		Argv: []string{"/bin/sh", "-c", `printf '%s' "$XDG_RUNTIME_DIR"`},
		Dir:  workDir,
		Env:  []string{"PATH=/usr/bin:/bin"},
		UID:  uid,
		GID:  gid,
	}, unit.ShellIO{
		Stdin:  bytes.NewReader(nil),
		Stdout: &shOut,
		Stderr: &shErr,
	})
	if err != nil {
		t.Fatalf("StartShell: %v", err)
	}
	code := h.Wait()
	_ = h.Close()
	if code != 0 {
		t.Fatalf("shell exit=%d stderr=%q", code, shErr.String())
	}
	if strings.TrimSpace(shOut.String()) != wantDir {
		t.Fatalf("shell XDG_RUNTIME_DIR = %q, want %q", shOut.String(), wantDir)
	}
	info, err = os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat %s after shell: %v", wantDir, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s perm after shell = %04o, want 0700", wantDir, got)
	}
}
