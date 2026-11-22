// Command hotbed is every part of a hotbed cluster: the controller, the
// workers, and the client that agents use.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], deps{
		tailnet: &tailnet.Local{},
		http:    &http.Client{Timeout: 60 * time.Second},
		stream:  &http.Client{},
		check:   worker.CheckMachine,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		cache:   cacheDir(),
		build:   version.Number(),
		exe:     os.Executable,
		reexec:  reexec,
	})
	stop()
	os.Exit(code)
}

// run parses args, runs the command they name, and prints its envelope to
// stdout. It returns the exit status.
func run(ctx context.Context, args []string, d deps) int {
	cmd, name, exitCode, err := parse(args, d.stdout)
	if exitCode != nil {
		return *exitCode
	}
	if err != nil {
		writeEnvelope(d.stdout, failure(name, invalidArgs(err)))
		return exitInvalidArgs
	}
	if c, ok := cmd.(interface{ controller() string }); ok && name != "upgrade" {
		var seen atomic.Int64
		d = watchBuild(d, &seen)
		defer func() { followController(context.WithoutCancel(ctx), d, c.controller(), seen.Load()) }()
	}
	if raw, ok := cmd.(rawCommand); ok {
		code, printed, err := raw.Raw(ctx, d)
		if err != nil {
			if printed {
				// The command's output is already on stdout; say what went
				// wrong where it cannot be mistaken for that output.
				fmt.Fprintf(d.stderr, "hotbed: %v\n", toAPIError(err))
				return exitError
			}
			writeEnvelope(d.stdout, failure(name, toAPIError(err)))
			return exitError
		}
		return code
	}
	data, err := cmd.Exec(ctx, d)
	if err != nil {
		writeEnvelope(d.stdout, failure(name, toAPIError(err)))
		return exitError
	}
	writeEnvelope(d.stdout, success(name, data))
	return exitOK
}

// cacheDir is where the CLI keeps per-session state, such as wait cursors.
func cacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "hotbed")
	}
	return filepath.Join(dir, "hotbed")
}
