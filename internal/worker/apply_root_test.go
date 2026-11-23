//go:build linux && systemdtest

// Run as root on a systemd machine, the test binary at a path the
// sandbox's binds do not shadow (/usr/local/bin):
//
//	sudo HOTBED_TEST_USER=$USER ./worker.test -test.run TestSyncAppliesInTheSandbox
package worker

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/unit"
)

// A sync applies chrooted in the workspace's rootfs as its user: the bundle is
// bound read-only where the rootfs sees it, owned so its user can read it, and
// the checkout is updated inside the chroot.
func TestSyncAppliesInTheChroot(t *testing.T) {
	name := os.Getenv("HOTBED_TEST_USER")
	if name == "" {
		t.Fatal("set HOTBED_TEST_USER to the user commands run as")
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// The rootfs: the host's users, its programs bound in read-only.
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"etc", "proc", "sys", "dev", "tmp", "work", "home/" + name} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"passwd", "group"} {
		b, err := os.ReadFile("/etc/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var progs []unit.Bind
	for _, d := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		fi, err := os.Lstat(d)
		switch {
		case err != nil:
		case fi.IsDir():
			progs = append(progs, unit.Bind{Source: d, Target: d})
		case fi.Mode()&os.ModeSymlink != 0: // merged /usr
			target, err := os.Readlink(d)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, d)); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The workspace's checkout, the user's on disk, with a planted hook.
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := t.TempDir()
	git(origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "lib.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(origin, "add", "-A")
	git(origin, "commit", "-qm", "base")
	base := git(origin, "rev-parse", "HEAD")
	checkout := filepath.Join(root, "work", "app")
	git(root, "clone", "-q", origin, checkout)
	hook := "#!/bin/sh\nid -u > /work/hook-ran\ntest -f /etc/chroot-hook-marker && echo chrooted >> /work/hook-ran\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "chroot-hook-marker"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Join(root, "work"), func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	}); err != nil {
		t.Fatal(err)
	}

	// What a client's sync sends: a commit changing lib.txt, as a bundle.
	if err := os.WriteFile(filepath.Join(origin, "lib.txt"), []byte("synced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(origin, "commit", "-qam", "sync")
	commit := git(origin, "rev-parse", "HEAD")
	git(origin, "update-ref", "refs/hotbed/sync", commit)
	bundlePath := filepath.Join(t.TempDir(), "sync.bundle")
	git(origin, "bundle", "create", "-q", bundlePath, base+"..refs/hotbed/sync")
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bundles"), 0o711); err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: EngineConfig{
		Dir: dir, User: name, Units: Systemd{},
		BaseImage: &baseimage.Manager{Spec: baseimage.Spec{User: name, UID: uid, GID: gid, Home: "/home/" + name}},
	}}
	received, _, err := e.receiveBundle("ws1", bytes.NewReader(bundle), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.applyConfig(root, checkout, received, commit, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ReadOnlyBinds = append(c.ReadOnlyBinds, progs...)
	n, err := e.runApply(t.Context(), c)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Errorf("apply changed %d files, want 1", n)
	}
	if b, err := os.ReadFile(filepath.Join(checkout, "lib.txt")); err != nil || string(b) != "synced\n" {
		t.Errorf("lib.txt = %q, %v; want synced", b, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "work", "hook-ran"))
	if err != nil {
		t.Fatalf("the planted hook did not run: %v", err)
	}
	if want := u.Uid + "\nchrooted\n"; string(b) != want {
		t.Errorf("the planted hook saw %q, want %q: the rootfs's user inside the chroot", b, want)
	}
}
