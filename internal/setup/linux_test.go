//go:build linux

package setup

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

func TestValidateLinuxMeta(t *testing.T) {
	baseDir := "/home/alice/.hotbed"
	validBase := LinuxSetupMeta{
		StateDir:    filepath.Join(baseDir, "worker"),
		User:        "alice",
		Controller:  "https://hotbed.example.com",
		StorageMode: "cladefs",
		ServiceName: "hotbed-worker.service",
	}

	if err := validateLinuxMeta(validBase, baseDir); err != nil {
		t.Fatalf("valid cladefs meta rejected: %v", err)
	}

	validShort := validBase
	validShort.ServiceName = "hotbed-worker"
	if err := validateLinuxMeta(validShort, baseDir); err != nil {
		t.Fatalf("valid short serviceName meta rejected: %v", err)
	}

	badCases := []struct {
		name   string
		mutate func(*LinuxSetupMeta)
	}{
		{name: "empty serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "" }},
		{name: "non-hotbed serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "sshd.service" }},
		{name: "flag-like serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "--root=/tmp" }},
		{name: "traversal serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "hotbed-../sshd.service" }},
		{name: "slash in serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "hotbed-a/b.service" }},
		{name: "shell metachar in serviceName", mutate: func(m *LinuxSetupMeta) { m.ServiceName = "hotbed-worker;rm" }},
		{name: "root stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "/" }},
		{name: "etc stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "/etc" }},
		{name: "baseDir itself as stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = baseDir }},
		{name: "relative stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "worker" }},
		{name: "unclean traversal stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = filepath.Join(baseDir, "worker") + "/../.." }},
		{name: "sibling prefix stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = baseDir + "2/worker" }},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			m := validBase
			tc.mutate(&m)
			if err := validateLinuxMeta(m, baseDir); err == nil {
				t.Fatalf("validateLinuxMeta(%+v) succeeded, want error", m)
			}
		})
	}
}

func TestVerifyTrustedMetaFile(t *testing.T) {
	myUID := uint32(os.Geteuid())

	t.Run("valid regular file and parent dir pass", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "meta")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "setup.json")
		if err := os.WriteFile(p, []byte(`{"serviceName":"hotbed-worker.service"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		var out LinuxSetupMeta
		if err := readTrustedJSON(p, myUID, &out); err != nil {
			t.Fatalf("readTrustedJSON valid file: %v", err)
		}
		if out.ServiceName != "hotbed-worker.service" {
			t.Fatalf("ServiceName = %q, want hotbed-worker.service", out.ServiceName)
		}
	})

	t.Run("wrong owner uid rejected", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "meta")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "setup.json")
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		wrongUID := myUID + 1
		if _, err := verifyTrustedMetaFile(p, wrongUID); err == nil {
			t.Fatalf("verifyTrustedMetaFile succeeded with wrong UID %d (actual %d)", wrongUID, myUID)
		}
	})

	t.Run("symlink file rejected", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "meta")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "real.json")
		if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "setup.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyTrustedMetaFile(link, myUID); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink file error = %v, want symlink rejection", err)
		}
	})

	t.Run("symlink parent directory rejected", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDir, "setup.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyTrustedMetaFile(filepath.Join(linkDir, "setup.json"), myUID); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink parent error = %v, want symlink rejection", err)
		}
	})

	t.Run("group-writable file rejected", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "meta")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "setup.json")
		if err := os.WriteFile(p, []byte(`{}`), 0o664); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o664); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyTrustedMetaFile(p, myUID); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("group-writable file error = %v, want writable rejection", err)
		}
	})

	t.Run("world-writable parent directory rejected", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "meta")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "setup.json")
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyTrustedMetaFile(p, myUID); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("world-writable dir error = %v, want writable rejection", err)
		}
	})
}

func TestResolveLinuxServiceBinaryFrom(t *testing.T) {
	myUID := uint32(os.Geteuid())

	newTrustedRoot := func(t *testing.T) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), "trusted-root")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("non-root-writable binary copied from self-exe source rather than reopening untrusted path", func(t *testing.T) {
		srcDir := filepath.Join(t.TempDir(), "userbin")
		if err := os.MkdirAll(srcDir, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(srcDir, 0o775); err != nil {
			t.Fatal(err)
		}
		// Simulate an attacker swapping the file at the untrusted path while
		// the running process's open /proc/self/exe still holds the original binary.
		untrustedPath := filepath.Join(srcDir, "hotbed")
		if err := os.WriteFile(untrustedPath, []byte("#!/bin/sh\necho attacker-swapped\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		selfExePath := filepath.Join(t.TempDir(), "proc-self-exe")
		wantBytes := []byte("#!/bin/sh\necho genuine-running-hotbed\n")
		if err := os.WriteFile(selfExePath, wantBytes, 0o755); err != nil {
			t.Fatal(err)
		}

		trustRoot := newTrustedRoot(t)
		dst := filepath.Join(trustRoot, "usr", "local", "bin", "hotbed")
		var progress bytes.Buffer

		got, err := resolveLinuxServiceBinaryFrom(untrustedPath, selfExePath, false, dst, trustRoot, myUID, &progress)
		if err != nil {
			t.Fatalf("resolveLinuxServiceBinaryFrom: %v", err)
		}
		if got != dst {
			t.Fatalf("resolved binary = %q, want %q", got, dst)
		}
		gotBytes, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read installed binary: %v", err)
		}
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Fatalf("installed binary content = %q, want %q (must copy from selfExePath, not untrusted path)", gotBytes, wantBytes)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("installed binary perm = %04o, want 0755", fi.Mode().Perm())
		}
		if !strings.Contains(progress.String(), dst) {
			t.Errorf("progress output %q did not mention %s", progress.String(), dst)
		}
	})

	t.Run("group-writable destination parent directory rejected", func(t *testing.T) {
		srcDir := filepath.Join(t.TempDir(), "userbin")
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(srcDir, "hotbed")
		if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}

		trustRoot := newTrustedRoot(t)
		badParent := filepath.Join(trustRoot, "usr", "local", "bin")
		if err := os.MkdirAll(badParent, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(badParent, 0o775); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(badParent, "hotbed")

		_, err := resolveLinuxServiceBinaryFrom(src, src, false, dst, trustRoot, myUID, nil)
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("group-writable parent error = %v, want writable rejection", err)
		}
		if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("binary should not be installed into untrusted dir; stat err = %v", statErr)
		}
	})

	t.Run("non-root-owned destination parent directory rejected", func(t *testing.T) {
		srcDir := filepath.Join(t.TempDir(), "userbin")
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(srcDir, "hotbed")
		if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}

		trustRoot := newTrustedRoot(t)
		dstDir := filepath.Join(trustRoot, "bin")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dstDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dstDir, "hotbed")

		// Require a different UID (simulating expectedUID=0 when dir is user-owned).
		_, err := resolveLinuxServiceBinaryFrom(src, src, false, dst, trustRoot, myUID+1, nil)
		if err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("wrong-owner parent error = %v, want owner rejection", err)
		}
		if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("binary should not be installed into wrong-owner dir; stat err = %v", statErr)
		}
	})

	t.Run("symlinked destination parent directory rejected", func(t *testing.T) {
		srcDir := filepath.Join(t.TempDir(), "userbin")
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(srcDir, "hotbed")
		if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}

		trustRoot := newTrustedRoot(t)
		realDir := filepath.Join(t.TempDir(), "realbin")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(trustRoot, "bin")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(linkDir, "hotbed")

		_, err := resolveLinuxServiceBinaryFrom(src, src, false, dst, trustRoot, myUID, nil)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlinked parent error = %v, want symlink rejection", err)
		}
		if _, statErr := os.Stat(filepath.Join(realDir, "hotbed")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("binary should not be installed through symlinked dir; stat err = %v", statErr)
		}
	})

	t.Run("root-owned non-writable binary kept in place when SELinux disabled", func(t *testing.T) {
		// /bin/sh or /usr/bin/env on Linux is owned by root and not group/world-writable.
		var rootBin string
		for _, candidate := range []string{"/usr/bin/env", "/bin/sh"} {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
				if unsafe, _ := pathWritableByNonRoot(resolved); !unsafe {
					rootBin = resolved
					break
				}
			}
		}
		if rootBin == "" {
			t.Skip("no root-owned non-writable system binary found")
		}
		trustRoot := newTrustedRoot(t)
		dst := filepath.Join(trustRoot, "should-not-exist")
		got, err := resolveLinuxServiceBinaryFrom(rootBin, rootBin, false, dst, trustRoot, myUID, nil)
		if err != nil {
			t.Fatalf("resolveLinuxServiceBinaryFrom(%q): %v", rootBin, err)
		}
		if got != rootBin {
			t.Fatalf("resolved binary = %q, want %q", got, rootBin)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("unexpectedly copied root-owned binary to %s", dst)
		}
	})
}

func TestDestroyLinuxWorkerAnchoredRemovalAndMetadataPreservation(t *testing.T) {
	t.Run("symlinked intermediate directory inside baseDir refuses removal and keeps outside files and metadata intact", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside")
		outsideWorker := filepath.Join(outside, "worker")
		if err := os.MkdirAll(outsideWorker, 0o755); err != nil {
			t.Fatal(err)
		}
		victimFile := filepath.Join(outsideWorker, "important.txt")
		if err := os.WriteFile(victimFile, []byte("do-not-delete"), 0o600); err != nil {
			t.Fatal(err)
		}

		baseDir := filepath.Join(t.TempDir(), "base")
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Intermediate symlink inside baseDir pointing outside baseDir.
		if err := os.Symlink(outside, filepath.Join(baseDir, "sub")); err != nil {
			t.Fatal(err)
		}

		unitDir := t.TempDir()
		metaPath := filepath.Join(t.TempDir(), "worker-meta.json")
		if err := os.WriteFile(metaPath, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := LinuxSetupMeta{
			StateDir:    filepath.Join(baseDir, "sub", "worker"),
			User:        "alice",
			Controller:  "https://hotbed.example.com",
			StorageMode: "cladefs",
			ServiceName: "hotbed-worker.service",
		}
		fakeRun := func(_ context.Context, _ string, _ ...string) (string, error) {
			return "", nil
		}

		_, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("destroyLinuxWorker err = %v, want symlink rejection", err)
		}
		if _, statErr := os.Stat(victimFile); statErr != nil {
			t.Errorf("outside file was deleted through symlinked ancestor: %v", statErr)
		}
		if _, statErr := os.Stat(metaPath); statErr != nil {
			t.Errorf("trusted metadata was deleted after failed cleanup: %v", statErr)
		}
	})

	t.Run("cleanup failure preserves trusted metadata file", func(t *testing.T) {
		baseDir := filepath.Join(t.TempDir(), "base")
		stateDir := filepath.Join(baseDir, "worker")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		unitDir := t.TempDir()
		svcPath := filepath.Join(unitDir, "hotbed-worker.service")
		if err := os.WriteFile(svcPath, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		metaPath := filepath.Join(t.TempDir(), "worker-meta.json")
		if err := os.WriteFile(metaPath, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := LinuxSetupMeta{
			StateDir:    stateDir,
			User:        "alice",
			Controller:  "https://hotbed.example.com",
			StorageMode: "cladefs",
			ServiceName: "hotbed-worker.service",
		}

		systemdErr := errors.New("systemd dbus unavailable")
		fakeRunSystemdFail := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "systemctl" && len(args) > 0 && args[0] == "disable" {
				return "", systemdErr
			}
			return "", nil
		}
		if _, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRunSystemdFail); !errors.Is(err, systemdErr) {
			t.Fatalf("destroyLinuxWorker systemd fail err = %v, want %v", err, systemdErr)
		}
		if _, statErr := os.Stat(metaPath); statErr != nil {
			t.Fatalf("trusted metadata was deleted despite systemctl failure: %v", statErr)
		}
		if _, statErr := os.Stat(stateDir); statErr != nil {
			t.Fatalf("stateDir was deleted despite earlier systemctl failure: %v", statErr)
		}
	})

	t.Run("successful cleanup removes unit, stateDir, and metadata", func(t *testing.T) {
		baseDir := filepath.Join(t.TempDir(), "base")
		stateDir := filepath.Join(baseDir, "worker")
		if err := os.MkdirAll(filepath.Join(stateDir, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "nested", "data"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		legacyMeta := legacyLinuxMetaPath(baseDir)
		if err := os.WriteFile(legacyMeta, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		unitDir := t.TempDir()
		svcPath := filepath.Join(unitDir, "hotbed-worker.service")
		if err := os.WriteFile(svcPath, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		metaPath := filepath.Join(t.TempDir(), "worker-meta.json")
		if err := os.WriteFile(metaPath, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := LinuxSetupMeta{
			StateDir:    stateDir,
			User:        "alice",
			Controller:  "https://hotbed.example.com",
			StorageMode: "cladefs",
			ServiceName: "hotbed-worker.service",
		}
		fakeRun := func(_ context.Context, _ string, _ ...string) (string, error) {
			return "", nil
		}
		res, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun)
		if err != nil {
			t.Fatalf("destroyLinuxWorker: %v", err)
		}
		for _, path := range []string{svcPath, stateDir, metaPath, legacyMeta} {
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("%s still exists after destroy: err=%v", path, statErr)
			}
		}
		for _, want := range []string{svcPath, stateDir, metaPath} {
			if !slices.Contains(res.Removed, want) {
				t.Errorf("Removed = %v, missing %q", res.Removed, want)
			}
		}
	})
}

func TestLinuxStateDirOwnershipAndPermissions(t *testing.T) {
	type chownCall struct {
		path string
		uid  int
		gid  int
	}

	t.Run("distinct baseDir and stateDir chowns baseDir to user and stateDir to root:root 0700", func(t *testing.T) {
		root := t.TempDir()
		baseDir := filepath.Join(root, "home", "alice", ".hotbed")
		stateDir := filepath.Join(baseDir, "worker")

		var calls []chownCall
		recChown := func(path string, uid, gid int) error {
			calls = append(calls, chownCall{path: filepath.Clean(path), uid: uid, gid: gid})
			return nil
		}
		cmdUser := &user.User{Uid: "1000", Gid: "1000", Username: "alice"}
		if err := prepareLinuxDirs(baseDir, stateDir, cmdUser, recChown); err != nil {
			t.Fatalf("prepareLinuxDirs: %v", err)
		}
		if err := enforceRootStateDir(stateDir, recChown); err != nil {
			t.Fatalf("enforceRootStateDir: %v", err)
		}

		baseFI, err := os.Stat(baseDir)
		if err != nil || baseFI.Mode().Perm() != 0o755 {
			t.Fatalf("baseDir perm = %04o (err=%v), want 0755", baseFI.Mode().Perm(), err)
		}
		stateFI, err := os.Stat(stateDir)
		if err != nil || stateFI.Mode().Perm() != 0o700 {
			t.Fatalf("stateDir perm = %04o (err=%v), want 0700", stateFI.Mode().Perm(), err)
		}
		if !slices.Contains(calls, chownCall{path: baseDir, uid: 1000, gid: 1000}) {
			t.Errorf("expected baseDir chowned to 1000:1000, calls=%v", calls)
		}
		if !slices.Contains(calls, chownCall{path: stateDir, uid: 0, gid: 0}) {
			t.Errorf("expected stateDir chowned to 0:0, calls=%v", calls)
		}
	})
}

func TestApplyLinuxSystemdState(t *testing.T) {
	t.Run("noStart does not enable or start worker service", func(t *testing.T) {
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return "", nil
		}
		running, err := applyLinuxSystemdState(t.Context(), true, linuxWorkerService, nil, fakeRun)
		if err != nil {
			t.Fatalf("applyLinuxSystemdState(noStart=true): %v", err)
		}
		if running {
			t.Fatal("expected running=false when noStart=true")
		}
		want := []string{"systemctl daemon-reload"}
		if !slices.Equal(cmds, want) {
			t.Fatalf("commands = %v, want %v", cmds, want)
		}
	})

	t.Run("default enables and restarts worker service", func(t *testing.T) {
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
				return "active\n", nil
			}
			return "", nil
		}
		running, err := applyLinuxSystemdState(t.Context(), false, linuxWorkerService, nil, fakeRun)
		if err != nil {
			t.Fatalf("applyLinuxSystemdState(noStart=false): %v", err)
		}
		if !running {
			t.Fatal("expected running=true")
		}
		want := []string{
			"systemctl daemon-reload",
			"systemctl enable " + linuxWorkerService,
			"systemctl restart " + linuxWorkerService,
			"systemctl is-active " + linuxWorkerService,
		}
		if !slices.Equal(cmds, want) {
			t.Fatalf("commands = %v, want %v", cmds, want)
		}
	})
}

// Setting up a host already joined to the controller reports the node name
// it joined under and asks the controller nothing (nil client and check
// panic if called).
func TestJoinLinuxWorkerAlreadyJoined(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := statedir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	const controller = "https://hotbed-controller.example.workers.dev"
	if err := statedir.Commit(dir, statedir.Config{Worker: &statedir.WorkerConfig{
		ClusterID: "c1", Controller: controller, User: "alice", Host: "box",
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := joinLinuxWorker(t.Context(), Options{Hostname: "ignored"}, dir, controller, "alice", nil, nil)
	if err != nil {
		t.Fatalf("joinLinuxWorker: %v", err)
	}
	want := worker.JoinResult{ClusterID: "c1", Dir: dir, Controller: controller, Storage: "cladefs", User: "alice", Node: "box"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("join result (-want +got):\n%s", diff)
	}
	if _, err := joinLinuxWorker(t.Context(), Options{}, dir, "https://other.example", "alice", nil, nil); api.CodeOf(err) != api.CodeAlreadyInitialized {
		t.Fatalf("joined to another controller: %v, want ALREADY_INITIALIZED", err)
	}
}
