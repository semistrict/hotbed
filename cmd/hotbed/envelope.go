package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/statedir"
)

// envelopeVersion changes when a field of the envelope, or of a command's
// data, changes meaning or goes away.
const envelopeVersion = "1"

const (
	exitOK          = 0
	exitError       = 1
	exitInvalidArgs = 2
	// exitPaused is how hotbed run and logs -f exit when the command's soft
	// time limit paused it, as timeout(1) exits when its limit passes.
	exitPaused = 124
	// exitInterrupted is how commands exit when stopped by Ctrl-C (SIGINT).
	exitInterrupted = 130
)

// tildePath shortens a path under the user's home directory to ~/..., as
// people write it.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return tildePathWithHome(p, home)
}

func tildePathWithHome(p, home string) string {
	if home == "" {
		return p
	}
	home = strings.TrimRight(home, string(os.PathSeparator))
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// envelope is the one JSON object every command prints.
type envelope struct {
	Version string     `json:"version"`
	OK      bool       `json:"ok"`
	Command string     `json:"command"`
	Data    any        `json:"data,omitempty"`
	Error   *api.Error `json:"error,omitempty"`
}

// success wraps a command's data; a protobuf message is written as
// protobuf JSON, the same field names the API uses.
func success(command string, data any) envelope {
	if m, ok := data.(proto.Message); ok {
		b, err := protojson.MarshalOptions{EmitDefaultValues: true}.Marshal(m)
		if err != nil {
			return failure(command, api.Errorf(api.CodeInternal, "encode output: %v", err))
		}
		data = json.RawMessage(b)
	}
	return envelope{Version: envelopeVersion, OK: true, Command: command, Data: data}
}

func failure(command string, e *api.Error) envelope {
	return envelope{Version: envelopeVersion, OK: false, Command: command, Error: e}
}

func writeEnvelope(w io.Writer, e envelope) {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		// Only a command's data can fail to encode; report that instead.
		b, _ = json.MarshalIndent(failure(e.Command, api.Errorf(api.CodeInternal, "encode output: %v", err)), "", "  ")
	}
	if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
		slog.Error("write output", "err", err)
	}
}

func invalidArgs(err error) *api.Error {
	var e *api.Error
	if errors.As(err, &e) {
		return e
	}
	return api.Errorf(api.CodeInvalidArgs, "%v", err).WithSuggestion("see hotbed --help, or hotbed COMMAND --help")
}

// toAPIError gives every failure a code: an *api.Error keeps its own, the
// state directory's errors get theirs, and anything else is INTERNAL.
func toAPIError(err error) *api.Error {
	var e *api.Error
	switch {
	case errors.As(err, &e):
		return e
	case isConnect(err):
		return api.FromConnect(err)
	case errors.Is(err, statedir.ErrAlreadyInitialized):
		return api.Errorf(api.CodeAlreadyInitialized, "%v", err).
			WithSuggestion("serve it with hotbed serve DIR, or pick another directory")
	case errors.Is(err, statedir.ErrNotInitialized):
		return api.Errorf(api.CodeNotInitialized, "%v", err).
			WithSuggestion("create it with hotbed join CONTROLLER DIR")
	default:
		return api.Errorf(api.CodeInternal, "%v", err)
	}
}

func isConnect(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce)
}
