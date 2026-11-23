package main

import (
	"io"
	"os"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
)

// jsonInput is where a command's JSON comes from: stdin, or --file.
type jsonInput struct {
	File string `type:"existingfile" help:"Read the JSON from this file instead of stdin."`
}

// read is the JSON; none fails, suggesting example.
func (in jsonInput) read(d deps, example string) ([]byte, error) {
	var (
		b   []byte
		err error
	)
	switch {
	case in.File != "":
		b, err = os.ReadFile(in.File)
	case d.stdin != nil:
		b, err = io.ReadAll(d.stdin)
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "no JSON on stdin").WithSuggestion(example)
	}
	return b, nil
}
