//go:build linux

package unit

import (
	"os"
	"path/filepath"
	"testing"
)

// Mountpoints are created inside the rootfs: no symlink there, to outside the
// rootfs or not, leads root to make or change anything outside it (board 8003).
func TestPrepareChrootStaysInsideTheRootfs(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "host-secret.txt")
	if err := os.WriteFile(outsideFile, []byte("host-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	srcFile := filepath.Join(t.TempDir(), "source.conf")
	if err := os.WriteFile(srcFile, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		link, to string // a symlink in the rootfs, relative to it
		bind     Bind
	}{
		"file target is a link out":  {"etc/target.conf", outsideFile, Bind{Source: srcFile, Target: "/etc/target.conf"}},
		"file parent is a link out":  {"var", outside, Bind{Source: srcFile, Target: "/var/escape/file.conf"}},
		"dir target is a link out":   {"tmp", outside, Bind{Source: srcDir, Target: "/tmp"}},
		"dir parent is a link out":   {"run", outside, Bind{Source: srcDir, Target: "/run/hotbed/git-auth"}},
		"/etc is a link out":         {"etc", outside, Bind{Source: srcFile, Target: "/etc/x.conf"}},
		"a relative link climbs out": {"opt", "../../../../../../../../" + outside, Bind{Source: srcDir, Target: "/opt/d"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, filepath.Dir(c.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.to, filepath.Join(root, c.link)); err != nil {
				t.Fatal(err)
			}
			_ = Spec{RootDir: root, Binds: []Bind{c.bind}}.prepareChroot()
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Errorf("outside the rootfs now holds %v (%v), want only host-secret.txt", entries, err)
			}
			if b, err := os.ReadFile(outsideFile); err != nil || string(b) != "host-secret" {
				t.Errorf("the file outside the rootfs changed: %q %v", b, err)
			}
		})
	}
	root := t.TempDir()
	if err := (Spec{RootDir: root, Binds: []Bind{{Source: srcDir, Target: "/run/hotbed/git-auth"}, {Source: srcFile, Target: "/etc/hotbed/file.conf"}}}).prepareChroot(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "run", "hotbed", "git-auth")); err != nil || !fi.IsDir() {
		t.Errorf("dir target: %v %v, want a directory", fi, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "etc", "hotbed", "file.conf")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("file target: %v %v, want a file", fi, err)
	}
}
