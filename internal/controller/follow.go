package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/version"
)

// followEvery is how often the controller looks for a newer build.
const followEvery = 2 * time.Minute

// Follower keeps the controller on CI's newest build: it fetches each new
// build of every platform into the cache, which the controller serves to its
// workers and clients, and installs its own over the controller.
type Follower struct {
	GitHub *update.GitHub
	Cache  update.Cache
	Exe    string // the controller's binary
	Build  int64  // the build it runs, 0 for a dev build
}

// Run checks every followEvery until ctx ends or a new build is installed,
// when it returns update.ErrRestart.
func (f *Follower) Run(ctx context.Context) error {
	t := time.NewTicker(followEvery)
	defer t.Stop()
	for {
		err := f.Check(ctx)
		switch {
		case errors.Is(err, update.ErrRestart):
			return err
		case err != nil && ctx.Err() == nil:
			slog.Error("follow CI's builds", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check installs CI's newest build if it is not the one running, returning
// update.ErrRestart, and otherwise makes sure the cache keeps the running
// build, and only it, for the workers.
func (f *Follower) Check(ctx context.Context) error {
	rel, err := f.GitHub.Latest(ctx)
	if err != nil {
		return err
	}
	if !f.Cache.Has(rel.Build) {
		if err := f.Cache.Put(ctx, f.GitHub, rel); err != nil {
			return err
		}
	}
	if rel.Build == f.Build {
		f.Cache.Prune(f.Build)
		return nil
	}
	path, sum, err := f.Cache.Binary(rel.Build, update.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return err
	}
	b, err := os.Open(path)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := update.Install(b, sum, f.Exe); err != nil {
		return fmt.Errorf("install build %d over %s: %w", rel.Build, f.Exe, err)
	}
	slog.Info("installed CI's newest build: restarting as it", "build", rel.Build, "was", version.String())
	return update.ErrRestart
}
