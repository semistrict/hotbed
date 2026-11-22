package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
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

	mu        sync.Mutex
	restarted int64         // non-zero once a new build was installed
	restart   chan struct{} // closed once a new build is installed
}

func (f *Follower) restartCh() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restart == nil {
		f.restart = make(chan struct{})
	}
	return f.restart
}

// Run checks every followEvery until ctx ends or a new build is installed
// (by a periodic check or an explicit Upgrade call), when it returns
// update.ErrRestart.
func (f *Follower) Run(ctx context.Context) error {
	restart := f.restartCh()
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
		case <-restart:
			return update.ErrRestart
		case <-t.C:
		}
	}
}

// Check installs CI's newest build if it is not the one running, returning
// update.ErrRestart, and otherwise makes sure the cache keeps the running
// build, and only it, for the workers.
func (f *Follower) Check(ctx context.Context) error {
	_, restarting, err := f.Upgrade(ctx)
	if err != nil {
		return err
	}
	if restarting {
		return update.ErrRestart
	}
	return nil
}

// Upgrade checks CI for the newest build now, installs it if it differs from
// f.Build, and wakes Run to restart the controller. Returns the build the
// controller is on (or restarting into) and whether a restart was triggered.
func (f *Follower) Upgrade(ctx context.Context) (build int64, restarting bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restarted != 0 {
		return f.restarted, true, nil
	}
	rel, err := f.GitHub.Latest(ctx)
	if err != nil {
		return 0, false, err
	}
	if !f.Cache.Has(rel.Build) {
		if err := f.Cache.Put(ctx, f.GitHub, rel); err != nil {
			return 0, false, err
		}
	}
	if rel.Build == f.Build {
		f.Cache.Prune(f.Build)
		return rel.Build, false, nil
	}
	path, sum, err := f.Cache.Binary(rel.Build, update.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return 0, false, err
	}
	b, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer b.Close()
	if err := update.Install(b, sum, f.Exe); err != nil {
		return 0, false, fmt.Errorf("install build %d over %s: %w", rel.Build, f.Exe, err)
	}
	slog.Info("installed CI's newest build: restarting as it", "build", rel.Build, "was", version.String())
	f.restarted = rel.Build
	if f.restart == nil {
		f.restart = make(chan struct{})
	}
	close(f.restart)
	return rel.Build, true, nil
}
