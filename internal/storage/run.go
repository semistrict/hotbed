package storage

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// run runs a filesystem tool and returns its stdout. A failure carries the
// tool's stderr, which is where btrfs and zfs say what went wrong.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
