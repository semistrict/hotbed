//go:build linux

package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/worker"
)

// zfsDiskByID is a dedicated ZFS disk's path as a VM guest sees one.
const zfsDiskByID = "/dev/disk/by-id/virtio-hotbed-zfs"

func TestValidateLinuxMeta(t *testing.T) {
	baseDir := "/home/alice/.hotbed"
	validBase := LinuxSetupMeta{
		StateDir:    filepath.Join(baseDir, "worker"),
		User:        "alice",
		Controller:  "ctrl.ts.net:7120",
		StorageMode: "zfs-file",
		PoolName:    "hotbed",
		ImagePath:   filepath.Join(baseDir, "zfs.img"),
		DiskSize:    DefaultDiskSize,
		ServiceName: "hotbed-worker.service",
		TrimTimer:   "hotbed-worker-trim.timer",
	}

	if err := validateLinuxMeta(validBase, baseDir); err != nil {
		t.Fatalf("valid zfs-file meta rejected: %v", err)
	}

	validBtrfs := LinuxSetupMeta{
		StateDir:    filepath.Join(baseDir, "worker"),
		User:        "alice",
		Controller:  "ctrl.ts.net:7120",
		StorageMode: "btrfs",
		ServiceName: "hotbed-worker",
	}
	if err := validateLinuxMeta(validBtrfs, baseDir); err != nil {
		t.Fatalf("valid btrfs meta rejected: %v", err)
	}

	validZFSDevice := LinuxSetupMeta{
		StateDir:    "/var/lib/hotbed",
		User:        "alice",
		Controller:  "ctrl.ts.net:7120",
		StorageMode: "zfs-device",
		PoolName:    "hotbed",
		ZFSDevice:   zfsDiskByID,
		ServiceName: "hotbed-worker.service",
		TrimTimer:   "hotbed-worker-trim.timer",
	}
	if err := validateLinuxMeta(validZFSDevice, "/var/lib/hotbed"); err != nil {
		t.Fatalf("valid zfs-device meta (stateDir == baseDir) rejected: %v", err)
	}
	validZFSDeviceSub := validZFSDevice
	validZFSDeviceSub.StateDir = filepath.Join(baseDir, "worker")
	if err := validateLinuxMeta(validZFSDeviceSub, baseDir); err != nil {
		t.Fatalf("valid zfs-device meta (subpath stateDir) rejected: %v", err)
	}

	badZFSDeviceCases := []struct {
		name   string
		mutate func(*LinuxSetupMeta)
	}{
		{name: "missing zfsDevice", mutate: func(m *LinuxSetupMeta) { m.ZFSDevice = "" }},
		{name: "relative zfsDevice", mutate: func(m *LinuxSetupMeta) { m.ZFSDevice = "dev/sdb" }},
		{name: "outside /dev zfsDevice", mutate: func(m *LinuxSetupMeta) { m.ZFSDevice = "/tmp/zfs.img" }},
		{name: "traversal zfsDevice", mutate: func(m *LinuxSetupMeta) { m.ZFSDevice = "/dev/../etc/passwd" }},
		{name: "missing poolName", mutate: func(m *LinuxSetupMeta) { m.PoolName = "" }},
	}
	for _, tc := range badZFSDeviceCases {
		t.Run("zfs-device/"+tc.name, func(t *testing.T) {
			m := validZFSDevice
			tc.mutate(&m)
			if err := validateLinuxMeta(m, "/var/lib/hotbed"); err == nil {
				t.Fatalf("validateLinuxMeta(%+v) succeeded, want error", m)
			}
		})
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
		{name: "non-hotbed trimTimer", mutate: func(m *LinuxSetupMeta) { m.TrimTimer = "apt-daily.timer" }},
		{name: "traversal trimTimer", mutate: func(m *LinuxSetupMeta) { m.TrimTimer = "hotbed-../evil.timer" }},
		{name: "wrong suffix trimTimer", mutate: func(m *LinuxSetupMeta) { m.TrimTimer = "hotbed-worker.service" }},
		{name: "invalid storageMode", mutate: func(m *LinuxSetupMeta) { m.StorageMode = "ext4" }},
		{name: "missing poolName for zfs-file", mutate: func(m *LinuxSetupMeta) { m.PoolName = "" }},
		{name: "flag-like poolName", mutate: func(m *LinuxSetupMeta) { m.PoolName = "-f" }},
		{name: "slash in poolName", mutate: func(m *LinuxSetupMeta) { m.PoolName = "rpool/data" }},
		{name: "traversal poolName", mutate: func(m *LinuxSetupMeta) { m.PoolName = "hotbed..evil" }},
		{name: "shell metachar in poolName", mutate: func(m *LinuxSetupMeta) { m.PoolName = "hotbed;id" }},
		{name: "root stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "/" }},
		{name: "etc stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "/etc" }},
		{name: "baseDir itself as stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = baseDir }},
		{name: "relative stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = "worker" }},
		{name: "unclean traversal stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = filepath.Join(baseDir, "worker") + "/../.." }},
		{name: "sibling prefix stateDir", mutate: func(m *LinuxSetupMeta) { m.StateDir = baseDir + "2/worker" }},
		{name: "missing imagePath for zfs-file", mutate: func(m *LinuxSetupMeta) { m.ImagePath = "" }},
		{name: "outside imagePath", mutate: func(m *LinuxSetupMeta) { m.ImagePath = "/etc/passwd" }},
		{name: "relative imagePath", mutate: func(m *LinuxSetupMeta) { m.ImagePath = "zfs.img" }},
		{name: "unclean traversal imagePath", mutate: func(m *LinuxSetupMeta) { m.ImagePath = baseDir + "/../zfs.img" }},
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
		outsideImg := filepath.Join(outside, "zfs.img")
		if err := os.WriteFile(outsideImg, []byte("do-not-delete-img"), 0o600); err != nil {
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
			Controller:  "ctrl.ts.net:7120",
			StorageMode: "zfs-file",
			PoolName:    "hotbed",
			ImagePath:   filepath.Join(baseDir, "sub", "zfs.img"),
			ServiceName: "hotbed-worker.service",
		}
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return "", errors.New("no such pool")
			}
			return "", nil
		}

		_, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("destroyLinuxWorker err = %v, want symlink rejection", err)
		}
		if _, statErr := os.Stat(victimFile); statErr != nil {
			t.Errorf("outside file was deleted through symlinked ancestor: %v", statErr)
		}
		if _, statErr := os.Stat(outsideImg); statErr != nil {
			t.Errorf("outside image was deleted through symlinked ancestor: %v", statErr)
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
		imgPath := filepath.Join(baseDir, "zfs.img")
		if err := os.WriteFile(imgPath, []byte("img"), 0o600); err != nil {
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
			Controller:  "ctrl.ts.net:7120",
			StorageMode: "zfs-file",
			PoolName:    "hotbed",
			ImagePath:   imgPath,
			ServiceName: "hotbed-worker.service",
		}

		// Fail on zpool destroy.
		zpoolErr := errors.New("pool is busy")
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return "hotbed\n", nil
			}
			if name == "zpool" && len(args) > 0 && args[0] == "destroy" {
				return "", zpoolErr
			}
			return "", nil
		}
		if _, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun); !errors.Is(err, zpoolErr) {
			t.Fatalf("destroyLinuxWorker err = %v, want %v", err, zpoolErr)
		}
		if _, statErr := os.Stat(metaPath); statErr != nil {
			t.Fatalf("trusted metadata was deleted despite zpool destroy failure: %v", statErr)
		}

		// Also verify systemctl disable failure preserves metadata and leaves storage untouched.
		if err := os.WriteFile(svcPath, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
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

	t.Run("successful cleanup removes units, pool, image, stateDir, and metadata", func(t *testing.T) {
		baseDir := filepath.Join(t.TempDir(), "base")
		stateDir := filepath.Join(baseDir, "worker")
		if err := os.MkdirAll(filepath.Join(stateDir, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "nested", "data"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		imgPath := filepath.Join(baseDir, "zfs.img")
		if err := os.WriteFile(imgPath, []byte("img"), 0o600); err != nil {
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
			Controller:  "ctrl.ts.net:7120",
			StorageMode: "zfs-file",
			PoolName:    "hotbed",
			ImagePath:   imgPath,
			ServiceName: "hotbed-worker.service",
		}
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return "hotbed\n", nil
			}
			return "", nil
		}
		res, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun)
		if err != nil {
			t.Fatalf("destroyLinuxWorker: %v", err)
		}
		for _, path := range []string{svcPath, imgPath, stateDir, metaPath, legacyMeta} {
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("%s still exists after destroy: err=%v", path, statErr)
			}
		}
		for _, want := range []string{svcPath, "zpool:hotbed", imgPath, stateDir, metaPath} {
			if !slices.Contains(res.Removed, want) {
				t.Errorf("Removed = %v, missing %q", res.Removed, want)
			}
		}
	})

	t.Run("zfs-device cleanup with stateDir == baseDir destroys pool and units without failing anchored removal", func(t *testing.T) {
		baseDir := filepath.Join(t.TempDir(), "hotbed")
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatal(err)
		}
		unitDir := t.TempDir()
		svcPath := filepath.Join(unitDir, linuxWorkerService)
		trimSvcPath := filepath.Join(unitDir, linuxTrimService)
		trimTmrPath := filepath.Join(unitDir, linuxTrimTimer)
		for _, p := range []string{svcPath, trimSvcPath, trimTmrPath} {
			if err := os.WriteFile(p, []byte("[Unit]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		metaPath := filepath.Join(t.TempDir(), "worker-meta.json")
		if err := os.WriteFile(metaPath, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := LinuxSetupMeta{
			StateDir:    baseDir,
			User:        "alice",
			Controller:  "ctrl.ts.net:7120",
			StorageMode: "zfs-device",
			PoolName:    "hotbed",
			ZFSDevice:   zfsDiskByID,
			ServiceName: linuxWorkerService,
			TrimTimer:   linuxTrimTimer,
		}
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return "hotbed\n", nil
			}
			return "", nil
		}
		res, err := destroyLinuxWorker(t.Context(), baseDir, meta, metaPath, unitDir, fakeRun)
		if err != nil {
			t.Fatalf("destroyLinuxWorker(zfs-device): %v", err)
		}
		for _, want := range []string{svcPath, trimTmrPath, trimSvcPath, "zpool:hotbed", metaPath} {
			if !slices.Contains(res.Removed, want) {
				t.Errorf("Removed = %v, missing %q", res.Removed, want)
			}
		}
	})
}

func TestEnsureLinuxZFSFileStorageMigratesLegacySetup(t *testing.T) {
	origMetaDir := linuxMetaDir
	t.Cleanup(func() { linuxMetaDir = origMetaDir })
	linuxMetaDir = filepath.Join(t.TempDir(), "etc-hotbed")
	if err := os.MkdirAll(linuxMetaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(linuxMetaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	myUID := uint32(os.Geteuid())
	// Simulate root running Setup (expectedMetaUID != baseDir owner UID) on a
	// pre-upgrade host where baseDir is owned by the command user and contains a
	// legacy setup.json, while /etc/hotbed/worker-*.json does not yet exist.
	simulatedRootUID := myUID + 1

	for _, tc := range []struct {
		name     string
		livePool string
	}{
		{name: "default hotbed pool", livePool: DefaultPoolName},
		{name: "custom live pool mounted at stateDir", livePool: "hotbed-legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := filepath.Join(t.TempDir(), "user-hotbed")
			stateDir := filepath.Join(baseDir, "worker")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// Legacy setup.json in the non-root-owned baseDir (even with tampered
			// poolName) must be ignored during migration, never causing a trust error.
			legacyPath := legacyLinuxMetaPath(baseDir)
			if err := os.WriteFile(legacyPath, []byte(`{"poolName":"evil-pool","stateDir":"/etc"}`), 0o644); err != nil {
				t.Fatal(err)
			}

			fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
				if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name"}) {
					return "rpool\n" + tc.livePool + "\n", nil
				}
				if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name", tc.livePool}) {
					return tc.livePool + "\n", nil
				}
				if name == "zfs" && slices.Equal(args, []string{"get", "-H", "-o", "value", "mountpoint", "rpool"}) {
					return "/\n", nil
				}
				if name == "zfs" && slices.Equal(args, []string{"get", "-H", "-o", "value", "mountpoint", tc.livePool}) {
					return stateDir + "\n", nil
				}
				if name == "zpool" && len(args) >= 2 && args[0] == "set" {
					return "", nil
				}
				return "", errors.New("unexpected command: " + name + " " + strings.Join(args, " "))
			}

			mode, pool, img, err := ensureLinuxZFSFileStorage(t.Context(), baseDir, stateDir, DefaultDiskSize, nil, simulatedRootUID, fakeRun)
			if err != nil {
				t.Fatalf("ensureLinuxZFSFileStorage migration failed: %v", err)
			}
			if mode != "zfs-file" || pool != tc.livePool || img != filepath.Join(baseDir, "zfs.img") {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", mode, pool, img, "zfs-file", tc.livePool, filepath.Join(baseDir, "zfs.img"))
			}
			// Legacy setup.json must remain intact until Setup writes the new
			// trusted metadata file and explicitly cleans it up.
			if _, err := os.Stat(legacyPath); err != nil {
				t.Fatalf("legacy setup.json was removed prematurely before trusted metadata was written: %v", err)
			}

			// Writing the new trusted metadata and cleaning up legacy setup.json
			// completes the migration.
			migrated := LinuxSetupMeta{
				StateDir:    stateDir,
				User:        "alice",
				Controller:  "ctrl.ts.net:7120",
				StorageMode: mode,
				PoolName:    pool,
				ImagePath:   img,
				DiskSize:    DefaultDiskSize,
				ServiceName: linuxWorkerService,
				TrimTimer:   linuxTrimTimer,
			}
			if err := writeTrustedJSONAtomic(linuxMetaPath(baseDir), migrated, 0o644); err != nil {
				t.Fatalf("writeTrustedJSONAtomic: %v", err)
			}
			removeLegacyMetaIfRegular(baseDir)
			if _, err := os.Stat(legacyPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("legacy setup.json still exists after migration cleanup: %v", err)
			}
			// Subsequent pool resolution reads the trusted metadata cleanly.
			gotPool, err := resolveLinuxPoolName(t.Context(), baseDir, stateDir, img, myUID, func(context.Context, string, ...string) (string, error) {
				t.Fatal("should not query live zpool when trusted metadata exists")
				return "", nil
			})
			if err != nil || gotPool != tc.livePool {
				t.Fatalf("post-migration resolveLinuxPoolName = (%q, %v), want (%q, nil)", gotPool, err, tc.livePool)
			}
		})
	}
}

type fakeDevFileInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeDevFileInfo) Name() string       { return f.name }
func (f fakeDevFileInfo) Size() int64        { return 0 }
func (f fakeDevFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeDevFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeDevFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeDevFileInfo) Sys() any           { return nil }

func stubZFSBlockDevice(t *testing.T, mapping map[string]string, modes map[string]fs.FileMode) {
	t.Helper()
	origEval := evalZFSDeviceSymlinks
	origStat := statZFSDevice
	t.Cleanup(func() {
		evalZFSDeviceSymlinks = origEval
		statZFSDevice = origStat
	})
	evalZFSDeviceSymlinks = func(p string) (string, error) {
		if target, ok := mapping[p]; ok {
			return target, nil
		}
		return "", fs.ErrNotExist
	}
	statZFSDevice = func(p string) (os.FileInfo, error) {
		if m, ok := modes[p]; ok {
			return fakeDevFileInfo{name: filepath.Base(p), mode: m}, nil
		}
		return nil, fs.ErrNotExist
	}
}

// fakeZFSModule points the zfs module's config, parameters and ARC stats at
// files in a temp dir; the ARC reports c_max as cMax.
func fakeZFSModule(t *testing.T, cMax int64) (conf, params string) {
	t.Helper()
	origConf, origParams, origStats := zfsModprobeConfPath, zfsParamDir, zfsARCStatsPath
	t.Cleanup(func() { zfsModprobeConfPath, zfsParamDir, zfsARCStatsPath = origConf, origParams, origStats })
	dir := t.TempDir()
	zfsModprobeConfPath = filepath.Join(dir, "hotbed-zfs.conf")
	zfsParamDir = filepath.Join(dir, "parameters")
	zfsARCStatsPath = filepath.Join(dir, "arcstats")
	if err := os.Mkdir(zfsParamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stats := fmt.Sprintf("13 1 0x01 147 39984 1 1\nname                            type data\nc_min                           4    785431296\nc_max                           4    %d\nsize                            4    5830038312\n", cMax)
	if err := os.WriteFile(zfsARCStatsPath, []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}
	return zfsModprobeConfPath, zfsParamDir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConfigureZFSModule(t *testing.T) {
	t.Run("caps the ARC now and from the next boot", func(t *testing.T) {
		conf, params := fakeZFSModule(t, 3<<30)
		if err := configureZFSModule(3 << 30); err != nil {
			t.Fatalf("configureZFSModule: %v", err)
		}
		if got, want := readFile(t, conf), "options zfs zfs_trim_txg_batch=1 zfs_arc_max=3221225472\n"; got != want {
			t.Errorf("modprobe conf = %q, want %q", got, want)
		}
		if got := readFile(t, filepath.Join(params, "zfs_arc_max")); got != "3221225472\n" {
			t.Errorf("zfs_arc_max = %q, want %q", got, "3221225472\n")
		}
		if got := readFile(t, filepath.Join(params, "zfs_trim_txg_batch")); got != "1\n" {
			t.Errorf("zfs_trim_txg_batch = %q, want %q", got, "1\n")
		}
	})

	t.Run("no cap sets zfs's default back", func(t *testing.T) {
		conf, params := fakeZFSModule(t, 24060059648)
		if err := configureZFSModule(0); err != nil {
			t.Fatalf("configureZFSModule: %v", err)
		}
		if got, want := readFile(t, conf), "options zfs zfs_trim_txg_batch=1\n"; got != want {
			t.Errorf("modprobe conf = %q, want %q", got, want)
		}
		if got := readFile(t, filepath.Join(params, "zfs_arc_max")); got != "0\n" {
			t.Errorf("zfs_arc_max = %q, want %q", got, "0\n")
		}
	})

	t.Run("a cap zfs ignores fails", func(t *testing.T) {
		fakeZFSModule(t, 24060059648)
		err := configureZFSModule(1000)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
			t.Fatalf("err = %v, want INVALID_ARGS *api.Error", err)
		}
		if want := "zfs kept its ARC cap at 22.41 GiB and ignored zfs_arc_max=1000 B"; apiErr.Message != want {
			t.Errorf("message = %q, want %q", apiErr.Message, want)
		}
	})
}

func TestEnsureLinuxZFSDeviceStorage(t *testing.T) {
	origMetaDir := linuxMetaDir
	t.Cleanup(func() { linuxMetaDir = origMetaDir })
	linuxMetaDir = filepath.Join(t.TempDir(), "etc-hotbed")
	if err := os.MkdirAll(linuxMetaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	myUID := uint32(os.Geteuid())
	blkidExit2Err := exec.Command("/bin/sh", "-c", "exit 2").Run()
	if blkidExit2Err == nil {
		t.Fatal("expected exit 2 error from /bin/sh")
	}

	stubDefaultDisk := func(t *testing.T) {
		t.Helper()
		stubZFSBlockDevice(t, map[string]string{
			zfsDiskByID:            "/dev/sdb",
			zfsDiskByID + "-part1": "/dev/sdb1",
			"/dev/sdb":             "/dev/sdb",
			"/dev/sdb1":            "/dev/sdb1",
			"/dev/sdc":             "/dev/sdc",
		}, map[string]fs.FileMode{
			"/dev/sdb":  os.ModeDevice,
			"/dev/sdb1": os.ModeDevice,
			"/dev/sdc":  os.ModeDevice,
		})
	}

	t.Run("rejects invalid device paths before running commands", func(t *testing.T) {
		for _, bad := range []string{"", "dev/sdb", "/tmp/disk.img", "/dev/../etc/passwd", "/dev/disk/../sdb"} {
			_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), "/var/lib/hotbed", "/var/lib/hotbed", bad, nil, myUID, func(context.Context, string, ...string) (string, error) {
				t.Fatalf("unexpected command execution for invalid device %q", bad)
				return "", nil
			})
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
				t.Errorf("device %q: err = %v, want INVALID_ARGS *api.Error", bad, err)
			}
		}
	})

	t.Run("validates block device existence, type, and symlink target", func(t *testing.T) {
		stubZFSBlockDevice(t, map[string]string{
			"/dev/null":        "/dev/null",
			"/dev/regular":     "/dev/regular",
			"/dev/escape-link": "/tmp/fake-disk",
			"/dev/dir":         "/dev/dir",
		}, map[string]fs.FileMode{
			"/dev/null":    os.ModeDevice | os.ModeCharDevice,
			"/dev/regular": 0o600,
			"/dev/dir":     os.ModeDir | 0o755,
		})
		for _, tc := range []struct {
			name string
			dev  string
		}{
			{name: "missing device", dev: "/dev/nonexistent"},
			{name: "character device", dev: "/dev/null"},
			{name: "regular file under /dev", dev: "/dev/regular"},
			{name: "directory under /dev", dev: "/dev/dir"},
			{name: "symlink escaping /dev", dev: "/dev/escape-link"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), "/var/lib/hotbed", "/var/lib/hotbed", tc.dev, nil, myUID, func(context.Context, string, ...string) (string, error) {
					t.Fatalf("unexpected command execution for non-block device %q", tc.dev)
					return "", nil
				})
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidArgs {
					t.Errorf("device %q: err = %v, want INVALID_ARGS *api.Error", tc.dev, err)
				}
			})
		}
	})

	t.Run("creates new ZFS pool on blank block device without -f and enforces 0700 stateDir", func(t *testing.T) {
		stubDefaultDisk(t)
		stateDir := filepath.Join(t.TempDir(), "hotbed")
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			cmds = append(cmds, cmd)
			if name == "zpool" && len(args) > 0 && (args[0] == "list" || args[0] == "import") {
				return "", errors.New("no pools available")
			}
			if name == "blkid" {
				return "", blkidExit2Err
			}
			return "", nil
		}

		mode, pool, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
		if err != nil {
			t.Fatalf("ensureLinuxZFSDeviceStorage: %v", err)
		}
		if mode != "zfs-device" || pool != DefaultPoolName {
			t.Fatalf("got (%q, %q), want (%q, %q)", mode, pool, "zfs-device", DefaultPoolName)
		}

		fi, err := os.Stat(stateDir)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("stateDir perm = %04o (err=%v), want 0700", fi.Mode().Perm(), err)
		}

		wantImport := "zpool import -d /dev/disk/by-id " + DefaultPoolName
		if !slices.Contains(cmds, wantImport) {
			t.Errorf("commands %v missing scoped import %q", cmds, wantImport)
		}
		wantCreate := "zpool create -o ashift=12 -o autotrim=on -o cachefile=/etc/zfs/zpool.cache -O compression=zstd -O atime=off -O xattr=sa -O dnodesize=auto -m " + stateDir + " " + DefaultPoolName + " " + zfsDiskByID
		if !slices.Contains(cmds, "modprobe zfs") {
			t.Errorf("commands %v missing modprobe zfs", cmds)
		}
		if !slices.Contains(cmds, wantCreate) {
			t.Errorf("commands %v missing %q", cmds, wantCreate)
		}
		if !slices.Contains(cmds, "systemctl enable zfs-import-cache.service zfs-mount.service zfs.target") {
			t.Errorf("commands %v missing zfs systemd enable", cmds)
		}
	})

	t.Run("refuses to wipe non-empty disk (existing filesystem, foreign ZFS label, or partition table)", func(t *testing.T) {
		stubDefaultDisk(t)
		for _, tc := range []struct {
			name   string
			fsType string
			ptType string
		}{
			{name: "ext4 filesystem", fsType: "ext4\n"},
			{name: "foreign zfs_member label", fsType: "zfs_member\n"},
			{name: "existing gpt partition table", ptType: "gpt\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stateDir := filepath.Join(t.TempDir(), "hotbed")
				fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
					if name == "zpool" && len(args) > 0 && (args[0] == "list" || args[0] == "import") {
						return "", errors.New("no pools available")
					}
					if name == "blkid" && slices.Contains(args, "TYPE") {
						if tc.fsType != "" {
							return tc.fsType, nil
						}
						return "", blkidExit2Err
					}
					if name == "blkid" && slices.Contains(args, "PTTYPE") {
						if tc.ptType != "" {
							return tc.ptType, nil
						}
						return "", blkidExit2Err
					}
					if name == "zpool" && len(args) > 0 && args[0] == "create" {
						t.Fatalf("should NOT run zpool create on non-empty disk: %v", args)
					}
					return "", nil
				}
				_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
					t.Fatalf("err = %v, want CONFLICT *api.Error", err)
				}
			})
		}
	})

	t.Run("reuses existing live pool backed by whole-disk partition and mounted at stateDir", func(t *testing.T) {
		stubDefaultDisk(t)
		stateDir := filepath.Join(t.TempDir(), "hotbed")
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name"}) {
				return DefaultPoolName + "\n", nil
			}
			if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name", DefaultPoolName}) {
				return DefaultPoolName + "\n", nil
			}
			if name == "zpool" && slices.Equal(args, []string{"list", "-v", "-H", "-P", DefaultPoolName}) {
				return DefaultPoolName + "\t64G\t1M\t64G\t-\t-\t0%\t0%\t1.00x\tONLINE\t-\n\t" + zfsDiskByID + "-part1\t64G\t1M\t64G\t-\t-\t0%\t0%\t-\tONLINE\n", nil
			}
			if name == "zfs" && slices.Equal(args, []string{"get", "-H", "-o", "value", "mountpoint", DefaultPoolName}) {
				return stateDir + "\n", nil
			}
			return "", nil
		}

		mode, pool, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
		if err != nil || mode != "zfs-device" || pool != DefaultPoolName {
			t.Fatalf("got (%q, %q, %v), want (%q, %q, nil)", mode, pool, err, "zfs-device", DefaultPoolName)
		}
		for _, c := range cmds {
			if strings.HasPrefix(c, "zpool create") {
				t.Fatalf("unexpected zpool create when pool already exists: %v", cmds)
			}
		}
		if !slices.Contains(cmds, "zpool set autotrim=on "+DefaultPoolName) {
			t.Errorf("commands %v missing zpool set autotrim=on", cmds)
		}
	})

	t.Run("rejects existing live pool backed by file image or different disk", func(t *testing.T) {
		stubDefaultDisk(t)
		for _, tc := range []struct {
			name string
			vdev string
		}{
			{name: "file-backed zfs.img", vdev: "/home/alice/.hotbed/zfs.img"},
			{name: "different block device", vdev: "/dev/sdc1"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stateDir := filepath.Join(t.TempDir(), "hotbed")
				fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
					if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name"}) {
						return DefaultPoolName + "\n", nil
					}
					if name == "zpool" && slices.Equal(args, []string{"list", "-H", "-o", "name", DefaultPoolName}) {
						return DefaultPoolName + "\n", nil
					}
					if name == "zpool" && slices.Equal(args, []string{"list", "-v", "-H", "-P", DefaultPoolName}) {
						return DefaultPoolName + "\t64G\t1M\t64G\t-\t-\t0%\t0%\t1.00x\tONLINE\t-\n\t" + tc.vdev + "\t64G\t1M\t64G\t-\t-\t0%\t0%\t-\tONLINE\n", nil
					}
					if name == "zfs" && len(args) > 0 && args[0] == "get" {
						return stateDir + "\n", nil
					}
					return "", nil
				}
				_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
					t.Fatalf("err = %v, want CONFLICT *api.Error", err)
				}
			})
		}
	})

	t.Run("existing live pool mounted elsewhere returns Conflict", func(t *testing.T) {
		stubDefaultDisk(t)
		stateDir := filepath.Join(t.TempDir(), "hotbed")
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && slices.Equal(args, []string{"list", "-v", "-H", "-P", DefaultPoolName}) {
				return DefaultPoolName + "\t64G\n\t/dev/sdb1\t64G\n", nil
			}
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return DefaultPoolName + "\n", nil
			}
			if name == "zfs" && len(args) > 0 && args[0] == "get" {
				return "/some/other/mount\n", nil
			}
			return "", nil
		}
		_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
			t.Fatalf("err = %v, want CONFLICT *api.Error", err)
		}
	})

	t.Run("imports exported pool scoped to device dir matching vdev and stateDir without -f", func(t *testing.T) {
		stubDefaultDisk(t)
		stateDir := filepath.Join(t.TempDir(), "hotbed")
		var imported bool
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && slices.Equal(args, []string{"list", "-v", "-H", "-P", DefaultPoolName}) {
				return DefaultPoolName + "\t64G\n\t/dev/sdb1\t64G\n", nil
			}
			if name == "zpool" && len(args) > 0 && args[0] == "list" {
				return "", errors.New("no pools")
			}
			if name == "zpool" && slices.Equal(args, []string{"import", "-d", "/dev/disk/by-id", DefaultPoolName}) {
				imported = true
				return "", nil
			}
			if name == "zfs" && slices.Equal(args, []string{"get", "-H", "-o", "value", "mountpoint", DefaultPoolName}) {
				return stateDir + "\n", nil
			}
			if name == "zpool" && len(args) > 0 && args[0] == "create" {
				t.Fatal("should not run zpool create when zpool import succeeded")
			}
			return "", nil
		}
		mode, pool, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
		if err != nil || !imported || mode != "zfs-device" || pool != DefaultPoolName {
			t.Fatalf("got (imported=%v, mode=%q, pool=%q, err=%v)", imported, mode, pool, err)
		}
	})

	t.Run("imported pool with vdev or mountpoint mismatch exports pool cleanly before returning Conflict", func(t *testing.T) {
		stubDefaultDisk(t)
		for _, tc := range []struct {
			name       string
			vdev       string
			mountpoint string
		}{
			{name: "vdev mismatch", vdev: "/dev/sdc1", mountpoint: ""},
			{name: "mountpoint mismatch", vdev: "/dev/sdb1", mountpoint: "/mnt/foreign"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stateDir := filepath.Join(t.TempDir(), "hotbed")
				mp := tc.mountpoint
				if mp == "" {
					mp = stateDir
				}
				var exported bool
				fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
					if name == "zpool" && slices.Equal(args, []string{"list", "-v", "-H", "-P", DefaultPoolName}) {
						return DefaultPoolName + "\t64G\n\t" + tc.vdev + "\t64G\n", nil
					}
					if name == "zpool" && len(args) > 0 && args[0] == "list" {
						return "", errors.New("no pools")
					}
					if name == "zpool" && slices.Equal(args, []string{"import", "-d", "/dev/disk/by-id", DefaultPoolName}) {
						return "", nil
					}
					if name == "zfs" && slices.Equal(args, []string{"get", "-H", "-o", "value", "mountpoint", DefaultPoolName}) {
						return mp + "\n", nil
					}
					if name == "zpool" && slices.Equal(args, []string{"export", DefaultPoolName}) {
						exported = true
						return "", nil
					}
					return "", nil
				}
				_, _, err := ensureLinuxZFSDeviceStorage(t.Context(), stateDir, stateDir, zfsDiskByID, nil, myUID, fakeRun)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.CodeConflict {
					t.Fatalf("err = %v, want CONFLICT *api.Error", err)
				}
				if !exported {
					t.Fatalf("expected zpool export %s on %s, but export was not called", DefaultPoolName, tc.name)
				}
			})
		}
	})
}

func TestLinuxStateDirOwnershipAndPermissions(t *testing.T) {
	origMetaDir := linuxMetaDir
	origChown := chownPath
	t.Cleanup(func() {
		linuxMetaDir = origMetaDir
		chownPath = origChown
	})
	linuxMetaDir = filepath.Join(t.TempDir(), "etc-hotbed")
	if err := os.MkdirAll(linuxMetaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	stubZFSBlockDevice(t, map[string]string{
		zfsDiskByID: "/dev/sdb",
		"/dev/sdb":  "/dev/sdb",
		"/dev/sdb1": "/dev/sdb1",
	}, map[string]fs.FileMode{
		"/dev/sdb":  os.ModeDevice,
		"/dev/sdb1": os.ModeDevice,
	})

	type chownCall struct {
		path string
		uid  int
		gid  int
	}

	t.Run("zfs-device with stateDir == baseDir never chowns stateDir to command user and enforces 0700 root:root", func(t *testing.T) {
		root := t.TempDir()
		baseDir := filepath.Join(root, "var", "lib", "hotbed")
		stateDir := resolveLinuxStateDir(baseDir, baseDir, zfsDiskByID)
		if stateDir != baseDir {
			t.Fatalf("resolveLinuxStateDir = %q, want %q", stateDir, baseDir)
		}

		var calls []chownCall
		chownPath = func(path string, uid, gid int) error {
			calls = append(calls, chownCall{path: filepath.Clean(path), uid: uid, gid: gid})
			return nil
		}
		cmdUser := &user.User{Uid: "1000", Gid: "1000", Username: "alice"}
		if err := prepareLinuxDirs(baseDir, stateDir, cmdUser, chownPath); err != nil {
			t.Fatalf("prepareLinuxDirs: %v", err)
		}
		// Parent directory strictly above stateDir must exist.
		if _, err := os.Stat(filepath.Dir(stateDir)); err != nil {
			t.Fatalf("parent of stateDir missing: %v", err)
		}

		blkidExit2Err := exec.Command("/bin/sh", "-c", "exit 2").Run()
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			if name == "zpool" && len(args) > 0 && (args[0] == "list" || args[0] == "import") {
				return "", errors.New("no pools")
			}
			if name == "blkid" {
				return "", blkidExit2Err
			}
			if name == "zpool" && len(args) > 0 && args[0] == "create" {
				// Simulate ZFS mounting a freshly created dataset with default 0755 perms.
				_ = os.Chmod(stateDir, 0o755)
			}
			return "", nil
		}
		if _, _, err := ensureLinuxZFSDeviceStorage(t.Context(), baseDir, stateDir, zfsDiskByID, nil, uint32(os.Geteuid()), fakeRun); err != nil {
			t.Fatalf("ensureLinuxZFSDeviceStorage: %v", err)
		}
		if err := enforceRootStateDir(stateDir, chownPath); err != nil {
			t.Fatalf("enforceRootStateDir: %v", err)
		}

		fi, err := os.Stat(stateDir)
		if err != nil {
			t.Fatalf("stat stateDir: %v", err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("stateDir perm = %04o, want 0700", fi.Mode().Perm())
		}
		var sawRootChown bool
		for _, c := range calls {
			if c.path == stateDir && (c.uid != 0 || c.gid != 0) {
				t.Fatalf("stateDir %s was chowned to non-root %d:%d (calls=%v)", stateDir, c.uid, c.gid, calls)
			}
			if c.path == stateDir && c.uid == 0 && c.gid == 0 {
				sawRootChown = true
			}
		}
		if !sawRootChown {
			t.Fatalf("expected stateDir %s to be chowned to 0:0, calls=%v", stateDir, calls)
		}
	})

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
	t.Run("noStart does not enable or start worker service or trim timer", func(t *testing.T) {
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return "", nil
		}
		running, err := applyLinuxSystemdState(t.Context(), true, linuxWorkerService, linuxTrimTimer, nil, fakeRun)
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

	t.Run("default enables and restarts worker service and enables trim timer", func(t *testing.T) {
		var cmds []string
		fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
				return "active\n", nil
			}
			return "", nil
		}
		running, err := applyLinuxSystemdState(t.Context(), false, linuxWorkerService, linuxTrimTimer, nil, fakeRun)
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
			"systemctl enable --now " + linuxTrimTimer,
			"systemctl is-active " + linuxWorkerService,
		}
		if !slices.Equal(cmds, want) {
			t.Fatalf("commands = %v, want %v", cmds, want)
		}
	})
}

func TestCleanupLegacyTrimUnits(t *testing.T) {
	unitDir := t.TempDir()
	var cmds []string
	fakeRun := func(_ context.Context, name string, args ...string) (string, error) {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return "", nil
	}

	// No legacy units: no systemctl call.
	cleanupLegacyTrimUnits(t.Context(), unitDir, fakeRun)
	if len(cmds) != 0 {
		t.Fatalf("unexpected commands when legacy units absent: %v", cmds)
	}

	// Legacy units present: disables timer and removes both files.
	legacySvc := filepath.Join(unitDir, legacyTrimService)
	legacyTmr := filepath.Join(unitDir, legacyTrimTimer)
	if err := os.WriteFile(legacySvc, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTmr, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanupLegacyTrimUnits(t.Context(), unitDir, fakeRun)
	if !slices.Contains(cmds, "systemctl disable --now "+legacyTrimTimer) {
		t.Errorf("commands %v missing disable of legacy timer", cmds)
	}
	for _, p := range []string{legacySvc, legacyTmr} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("legacy unit %s still exists: err=%v", p, err)
		}
	}
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
	got, err := joinLinuxWorker(t.Context(), Options{Hostname: "ignored"}, dir, controller, "alice", "zfs-file", nil, nil)
	if err != nil {
		t.Fatalf("joinLinuxWorker: %v", err)
	}
	want := worker.JoinResult{ClusterID: "c1", Dir: dir, Controller: controller, Storage: "zfs-file", User: "alice", Node: "box"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("join result (-want +got):\n%s", diff)
	}
	if _, err := joinLinuxWorker(t.Context(), Options{}, dir, "https://other.example", "alice", "zfs-file", nil, nil); api.CodeOf(err) != api.CodeAlreadyInitialized {
		t.Fatalf("joined to another controller: %v, want ALREADY_INITIALIZED", err)
	}
}
