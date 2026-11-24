package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestFixDevPermissions(t *testing.T) {
	t.Run("chmods all present device nodes to 0666", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{"fuse", "userfaultfd", "kvm", "net/tun"} {
			p := filepath.Join(dir, rel)
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		fixDevPermissions(log, dir)

		if logs.Len() != 0 {
			t.Fatalf("unexpected warnings: %s", logs.String())
		}
		for _, rel := range []string{"fuse", "userfaultfd", "kvm", "net/tun"} {
			p := filepath.Join(dir, rel)
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", rel, err)
			}
			if got := info.Mode().Perm(); got != 0o666 {
				t.Errorf("%s mode = %04o, want 0666", rel, got)
			}
		}
	})

	t.Run("skips missing nodes cleanly without warnings", func(t *testing.T) {
		dir := t.TempDir()
		// Create only fuse and userfaultfd; leave kvm and net/tun (and net/) absent.
		for _, rel := range []string{"fuse", "userfaultfd"} {
			if err := os.WriteFile(filepath.Join(dir, rel), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		fixDevPermissions(log, dir)

		if logs.Len() != 0 {
			t.Fatalf("unexpected warnings for ENOENT nodes: %s", logs.String())
		}
		for _, rel := range []string{"fuse", "userfaultfd"} {
			info, err := os.Stat(filepath.Join(dir, rel))
			if err != nil {
				t.Fatalf("stat %s: %v", rel, err)
			}
			if got := info.Mode().Perm(); got != 0o666 {
				t.Errorf("%s mode = %04o, want 0666", rel, got)
			}
		}
	})
}
