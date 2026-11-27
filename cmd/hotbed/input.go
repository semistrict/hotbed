package main

import (
	"io"
	"os"
)

// readFile reads a file a command names, "-" for stdin: its JSON, or a file
// flag's (--body-file).
func (d deps) readFile(path string) ([]byte, error) {
	if path != "-" {
		return os.ReadFile(path)
	}
	if d.stdin == nil {
		return nil, nil
	}
	return io.ReadAll(d.stdin)
}
