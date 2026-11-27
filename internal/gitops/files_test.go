package gitops

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A diff is a git the checkout's owner may have replaced: hunks
// mark only the file's own lines, once each, however many they claim.
func TestParseChangedLinesAreTheFilesOwn(t *testing.T) {
	for name, c := range map[string]struct {
		diff string
		n    int
		want []int32
	}{
		"a hunk":                     {"@@ -1 +2,2 @@\n", 5, []int32{2, 3}},
		"a deletion marks nothing":   {"@@ -3 +2,0 @@\n", 5, []int32{}},
		"a hunk claiming 2^31 lines": {"@@ -1 +1,2147483647 @@\n", 3, []int32{1, 2, 3}},
		"overlapping hunks":          {"@@ -1 +1,3 @@\n@@ -1 +2,3 @@\n", 9, []int32{1, 2, 3, 4}},
		"a hunk past the end":        {"@@ -1 +7,2 @@\n", 5, []int32{}},
		"a count past int":           {"@@ -1 +1,99999999999999999999 @@\n", 3, []int32{}},
	} {
		if got := ParseChangedLines(c.diff, c.n); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestCountLines(t *testing.T) {
	for text, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n": 1} {
		if got := CountLines([]byte(text)); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", text, got, want)
		}
	}
}

// A hunk after a line longer than a bufio.Scanner's is still read.
func TestParseChangedLinesPastALongLine(t *testing.T) {
	diff := "@@ -1 +1 @@\n-x\n+" + strings.Repeat("m", 200<<10) + "\n@@ -3 +3 @@\n-y\n+z\n"
	if got, want := ParseChangedLines(diff, 3), []int32{1, 3}; !slices.Equal(got, want) {
		t.Errorf("ParseChangedLines = %v, want %v", got, want)
	}
}

// A workspace's file read never blocks on a FIFO an agent put where a file
// was, nor reads through a symlink out of the checkout.
func TestReadRepoFileRefusesSpecialFilesAndEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f.txt", filepath.Join(dir, "in")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan error, 1)
	go func() { _, err := ReadRepoFile(root, "pipe", 1<<20); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("FIFO: %v, want ErrNotRegularFile", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
	if _, err := ReadRepoFile(root, "out", 1<<20); err == nil {
		t.Fatal("read through a symlink out of the checkout")
	}
	if _, err := ReadRepoFile(root, ".", 1<<20); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("directory: %v, want ErrNotRegularFile", err)
	}
	got, err := ReadRepoFile(root, "in", 3)
	if err != nil || string(got) != "hel" {
		t.Fatalf("symlink within the checkout, 3 bytes: %q, %v; want \"hel\"", got, err)
	}
}

// Files are listed with the directories that hold them, marked against the
// base; with no base, none is marked.
func TestListRepoFilesMarksChangesAgainstBase(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a\n")
	write("sub/b.txt", "b\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	base := git("rev-parse", "HEAD")
	write("a.txt", "a2\n")
	write("sub/c.txt", "c\n")
	git("add", ".")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	describe := func(base string) []string {
		t.Helper()
		files, err := ListRepoFiles(t.Context(), Git{Dir: dir}, root, base)
		if err != nil {
			t.Fatalf("ListRepoFiles: %v", err)
		}
		var out []string
		for _, f := range files {
			out = append(out, f.Kind+" "+f.Path+" "+f.Change)
		}
		slices.Sort(out)
		return out
	}
	if got, want := describe(base), []string{"dir sub ", "file a.txt modified", "file sub/b.txt ", "file sub/c.txt added"}; !slices.Equal(got, want) {
		t.Errorf("against base: %q, want %q", got, want)
	}
	if got, want := describe(""), []string{"dir sub ", "file a.txt ", "file sub/b.txt ", "file sub/c.txt "}; !slices.Equal(got, want) {
		t.Errorf("no base: %q, want %q", got, want)
	}
	changed, err := FileChangedLines(t.Context(), Git{Dir: dir}, "a.txt", base, []byte("a2\n"))
	if err != nil || !slices.Equal(changed, []int32{1}) {
		t.Errorf("FileChangedLines = %v, %v; want [1]", changed, err)
	}
}

// The credential helper reads the socket from the environment, or the
// default it was given, and never runs it as shell.
func TestGitCredentialHelperScriptSocket(t *testing.T) {
	if s := GitCredentialHelperScript(""); !strings.Contains(s, `--unix-socket "$HOTBED_GIT_CRED_SOCK" `) {
		t.Errorf("no default: %s", s)
	}
	if s := GitCredentialHelperScript("/run/hotbed/git-auth/git-cred.sock"); !strings.Contains(s, `--unix-socket "${HOTBED_GIT_CRED_SOCK:-/run/hotbed/git-auth/git-cred.sock}" `) {
		t.Errorf("default: %s", s)
	}
	if out, err := exec.Command("sh", "-n", "-c", GitCredentialHelperScript("/x")).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}
}
