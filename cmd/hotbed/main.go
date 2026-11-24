// Command hotbed is every part of a hotbed cluster: the controller, the
// workers, and the client that agents use.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/itchyny/gojq"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cliauth"
	"github.com/semistrict/hotbed/internal/setup"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/internal/worker"
)

func main() {
	// Internal worker subcommands (e.g. __sync-apply) exit here before anything else.
	worker.Main(os.Args[1:])
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], deps{
		tailnet:      &tailnet.Local{},
		http:         api.NewResilientHTTPClient(60 * time.Second),
		stream:       &http.Client{},
		rideOut:      peerRideOut,
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

// run parses args, runs the command they name, and prints its output to
// stdout. It returns the exit status.
func run(ctx context.Context, args []string, d deps) int {
	cmd, name, opts, exitCode, err := parse(args, d.stdout)
	if exitCode != nil {
		return *exitCode
	}
	if err != nil {
		writeFailure(d.stdout, d.stderr, name, invalidArgs(err), opts)
		return exitInvalidArgs
	}
	if opts.JQ != "" {
		if _, err := gojq.Parse(opts.JQ); err != nil {
			writeFailure(d.stdout, d.stderr, name, api.Errorf(api.CodeInvalidArgs, "invalid --jq expression: %v", err), opts)
			return exitInvalidArgs
		}
	}
	d.output = opts
	if c, ok := cmd.(interface{ callController(addr string, d deps) }); ok {
		addr, err := d.controller()
		if err != nil {
			writeFailure(d.stdout, d.stderr, name, toAPIError(err), opts)
			return exitError
		}
		c.callController(addr, d)
		if d.cockpit, err = newCockpit(addr); err != nil {
			writeFailure(d.stdout, d.stderr, name, toAPIError(err), opts)
			return exitError
		}
		d = watchProgress(d)
		if name != "upgrade" && name != "admin upgrade" {
			var seen atomic.Int64
			d = watchBuild(d, &seen)
			defer func() { followController(context.WithoutCancel(ctx), d, addr, seen.Load()) }()
		}
	}
	if raw, ok := cmd.(rawCommand); ok {
		code, printed, err := raw.Raw(ctx, d)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return exitInterrupted
			}
			if printed {
				// The command's output is already on stdout; say what went
				// wrong where it cannot be mistaken for that output.
				fmt.Fprintf(d.stderr, "hotbed: %v\n", toAPIError(err))
				return exitError
			}
			writeFailure(d.stdout, d.stderr, name, toAPIError(err), opts)
			return exitError
		}
		return code
	}
	data, err := cmd.Exec(ctx, d)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return exitInterrupted
		}
		writeFailure(d.stdout, d.stderr, name, toAPIError(err), opts)
		return exitError
	}
	d.cockpit.fill(data)
	if err := writeOutput(d.stdout, name, cmd, data, opts); err != nil {
		writeFailure(d.stdout, d.stderr, name, toAPIError(err), opts)
		return exitError
	}
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
// HOTBED_CONTROLLER, the controller `hotbed worker init` joined it to, or the
// default controller recorded by `hotbed login`.
func localController() (string, error) {
	if addr := os.Getenv("HOTBED_CONTROLLER"); addr != "" {
		return addr, nil
	}
	addr, err := setup.JoinedController("")
	if err != nil {
		return "", err
	}
	if addr != "" {
		return addr, nil
	}
	if dir, derr := defaultHotbedDir(); derr == nil {
		if def := cliauth.DefaultController(dir); def != "" {
			return def, nil
		}
	}
	return "", api.Errorf(api.CodeNotInitialized, "this machine has no controller").
		WithSuggestion("run `hotbed login https://...`, `hotbed worker init CONTROLLER`, or set HOTBED_CONTROLLER")
}
