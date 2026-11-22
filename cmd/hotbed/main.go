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

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], deps{
		tailnet:      &tailnet.Local{},
		http:         &http.Client{Timeout: 60 * time.Second},
		stream:       &http.Client{},
		check:        worker.CheckMachine,
		stdin:        os.Stdin,
		controller:   localController,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		cache:        cacheDir(),
		sessionsFile: defaultSessionsFile(),
		build:        version.Number(),
		exe:          os.Executable,
		reexec:       reexec,
		execve:       syscall.Exec,
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
	if c, ok := cmd.(interface{ callController(addr string) }); ok {
		addr, err := d.controller()
		if err != nil {
			writeEnvelope(d.stdout, failure(name, toAPIError(err)))
			return exitError
		}
		c.callController(addr)
		if name != "upgrade" && name != "admin upgrade" {
			var seen atomic.Int64
			d = watchBuild(d, &seen)
			defer func() { followController(context.WithoutCancel(ctx), d, addr, seen.Load()) }()
		}
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

// localController is the controller this machine's client commands call:
// the one `hotbed worker init` joined it to, or HOTBED_CONTROLLER on a
// machine that is no worker.
func localController() (string, error) {
	if addr := os.Getenv("HOTBED_CONTROLLER"); addr != "" {
		return addr, nil
	}
	addr, err := setup.JoinedController("")
	if err != nil {
		return "", err
	}
	if addr == "" {
		return "", api.Errorf(api.CodeNotInitialized, "this machine has no controller").
			WithSuggestion("run `hotbed worker init CONTROLLER` once on this machine, or set HOTBED_CONTROLLER on one that is no worker")
	}
	return addr, nil
}
