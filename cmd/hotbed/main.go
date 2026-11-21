// Command hotbed is every part of a hotbed cluster: the controller, the
// workers, and the client that agents use.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, deps{
		tailnet: &tailnet.Local{},
		http:    &http.Client{Timeout: 30 * time.Second},
		probe:   worker.Probe,
	})
	stop()
	os.Exit(code)
}

// run parses args, runs the command they name, and prints its envelope to
// stdout. It returns the exit status.
func run(ctx context.Context, args []string, stdout io.Writer, d deps) int {
	cmd, name, exitCode, err := parse(args, stdout)
	if exitCode != nil {
		return *exitCode
	}
	if err != nil {
		writeEnvelope(stdout, failure(name, invalidArgs(err)))
		return exitInvalidArgs
	}
	data, err := cmd.Exec(ctx, d)
	if err != nil {
		writeEnvelope(stdout, failure(name, toAPIError(err)))
		return exitError
	}
	writeEnvelope(stdout, success(name, data))
	return exitOK
}
