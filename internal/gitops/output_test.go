package gitops

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// shGit is a Git whose "git" is script: a git a sandbox replaced.
func shGit(script string) Git {
	return Git{Dir: "/", Command: func(ctx context.Context, _ string, _ []string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", script), nil
	}}
}

// A git that writes without end fills no more of the worker's memory than
// maxOutput: Run and Lines kill it once it writes more, and fail.
func TestGitOutputIsBounded(t *testing.T) {
	g := shGit("exec yes")
	want := "git x wrote over 134217728 bytes"
	if _, err := g.Run(t.Context(), "x"); err == nil || err.Error() != want {
		t.Errorf("Run: %v, want %q", err, want)
	}
	if _, err := g.Lines(t.Context(), "x"); err == nil || err.Error() != want {
		t.Errorf("Lines: %v, want %q", err, want)
	}
}

// What a failing git says on stderr is kept to maxStderr in its error.
func TestGitStderrIsBounded(t *testing.T) {
	_, err := shGit("head -c 1048576 /dev/zero | tr '\\0' q >&2; exit 1").Run(t.Context(), "x")
	if err == nil {
		t.Fatal("Run succeeded")
	}
	if got := strings.Count(err.Error(), "q"); got != maxStderr {
		t.Errorf("error kept %d bytes of stderr, want %d", got, maxStderr)
	}
}
