package cmdlog

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

func ttyPath(path string) string { return path + ".tty" }

// WriteTTYCols records the terminal column width used for the log at path so
// settled queries (internal/outq, GetLogLines) wrap at the PTY's width even
// after the worker has forgotten the ended command record.
func WriteTTYCols(path string, cols int) error {
	if path == "" || cols <= 0 {
		return nil
	}
	return os.WriteFile(ttyPath(path), []byte(strconv.Itoa(cols)+"\n"), 0o644)
}

// ReadTTYCols returns the recorded terminal column width for the log at path,
// or 0 if the command did not run in a TTY (or recorded none).
func ReadTTYCols(path string) int {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(ttyPath(path))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return 0
		}
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
