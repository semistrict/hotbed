package update_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

func sum(b []byte) string { return updatetest.Sum(b) }

// Install replaces a binary in one rename, executable, and only with what
// matches its sum.
func TestInstall(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "hotbed")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := updatetest.Binary("new")
	if err := update.Install(bytes.NewReader(newBin), sum([]byte("other")), exe); err == nil {
		t.Fatal("Install took a binary whose sum doesn't match")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("a refused Install left %q", b)
	}
	if err := update.Install(bytes.NewReader(newBin), sum(newBin), exe); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, newBin) || info.Mode().Perm() != 0o755 {
		t.Errorf("after Install: %q, mode %v", b, info.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("Install left %v beside the binary", entries)
	}
	if !update.Matches(exe, sum(newBin)) || update.Matches(exe, sum([]byte("other"))) || update.Matches(exe, "") {
		t.Errorf("Matches gave unexpected results for %s", exe)
	}
	// Installing identical bytes again leaves exe's inode untouched so file
	// watchers do not see a spurious replacement.
	if err := update.Install(bytes.NewReader(newBin), sum(newBin), exe); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, after) {
		t.Errorf("Install with identical content replaced exe's inode")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("no-op Install left %v beside the binary", entries)
	}
}

// Install keeps the binary a machine runs when handed any other platform's
// build, however well its sum matches: a Mac worker handed the Linux build
// once replaced its hotbed CLI and daemon with one it could not execute.
func TestInstallRefusesAnotherPlatformsBinary(t *testing.T) {
	here := update.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	for _, p := range update.Platforms {
		if p == here {
			continue
		}
		t.Run(p.String(), func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "hotbed")
			if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			b := updatetest.BinaryFor(p, "other platform")
			err := update.Install(bytes.NewReader(b), sum(b), exe)
			want := "the new binary is built for " + p.String() + ", not for this " + here.String() + " machine"
			if err == nil || err.Error() != want {
				t.Fatalf("Install(%s binary) = %v, want %q", p, err, want)
			}
			if got, _ := os.ReadFile(exe); string(got) != "old" {
				t.Fatalf("a refused Install left %q", got)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("a refused Install left %v beside the binary", entries)
			}
		})
	}
	t.Run("a real binary for this machine", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := update.RunsHere(self); err != nil {
			t.Fatalf("RunsHere(this test binary) = %v", err)
		}
	})
	t.Run("not an executable", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "hotbed")
		b := []byte("#!/bin/sh\necho not a hotbed build\n")
		err := update.Install(bytes.NewReader(b), sum(b), exe)
		if err == nil || err.Error() != "the new binary is neither an ELF nor a Mach-O executable" {
			t.Fatalf("Install(script) = %v", err)
		}
	})
}
