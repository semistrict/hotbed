package controller

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

func follower(t *testing.T, g *updatetest.GitHub, build int64) (*Follower, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "hotbed")
	if err := os.WriteFile(exe, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Follower{GitHub: g.Client(), Cache: update.Cache{Dir: t.TempDir()}, Exe: exe, Build: build}, exe
}

// A newer build is fetched, every platform's, installed over the
// controller, which restarts as it.
func TestFollowerInstallsANewerBuild(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(7)
	g.Publish(8)
	f, exe := follower(t, g, 7)
	if err := f.Check(t.Context()); !errors.Is(err, update.ErrRestart) {
		t.Fatalf("Check = %v, want a restart", err)
	}
	want := "hotbed build 8 for " + runtime.GOOS + "/" + runtime.GOARCH
	if b, _ := os.ReadFile(exe); string(b) != want {
		t.Errorf("the controller's binary is %q, want %q", b, want)
	}
	if !f.Cache.Has(8) {
		t.Error("the cache doesn't keep build 8, which workers will ask for")
	}
}

// On the newest build, it keeps that one to serve, and only that one.
func TestFollowerKeepsItsOwnBuild(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(7)
	g.Publish(8)
	f, exe := follower(t, g, 8)
	if err := f.Cache.Put(t.Context(), g.Client(), mustRelease(t, f.GitHub, 7)); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(t.Context()); err != nil {
		t.Fatalf("Check = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "running" {
		t.Errorf("the controller's binary changed to %q", b)
	}
	if !f.Cache.Has(8) || f.Cache.Has(7) {
		t.Errorf("the cache has 8 %v, 7 %v, want only its own build, 8", f.Cache.Has(8), f.Cache.Has(7))
	}
}

// A dev build follows CI's newest build too: nothing is deployed by hand.
func TestFollowerReplacesADevBuild(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(3)
	f, _ := follower(t, g, 0)
	if err := f.Check(t.Context()); !errors.Is(err, update.ErrRestart) {
		t.Fatalf("Check = %v, want a restart", err)
	}
}

// An explicit Upgrade wakes a sleeping Run immediately to restart as the new
// build, without waiting for followEvery.
func TestFollowerUpgradeWakesRun(t *testing.T) {
	g := updatetest.NewGitHub(t)
	g.Publish(8)
	f, exe := follower(t, g, 8)

	done := make(chan error, 1)
	go func() { done <- f.Run(t.Context()) }()

	// Wait until Run's initial check has cached build 8 and gone to sleep.
	deadline := time.Now().Add(5 * time.Second)
	for !f.Cache.Has(8) {
		if time.Now().After(deadline) {
			t.Fatal("Run never finished its initial check")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Already on 8: Upgrade does not trigger a restart.
	b, restarting, err := f.Upgrade(t.Context())
	if err != nil || b != 8 || restarting {
		t.Fatalf("Upgrade on latest = (%d, %v, %v), want (8, false, nil)", b, restarting, err)
	}

	g.Publish(9)
	b, restarting, err = f.Upgrade(t.Context())
	if err != nil || b != 9 || !restarting {
		t.Fatalf("Upgrade after publish = (%d, %v, %v), want (9, true, nil)", b, restarting, err)
	}
	// A repeat call while restarting is idempotent.
	b2, restarting2, err2 := f.Upgrade(t.Context())
	if err2 != nil || b2 != 9 || !restarting2 {
		t.Fatalf("repeat Upgrade = (%d, %v, %v), want (9, true, nil)", b2, restarting2, err2)
	}
	select {
	case err := <-done:
		if !errors.Is(err, update.ErrRestart) {
			t.Fatalf("Run ended with %v, want update.ErrRestart", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not wake on Upgrade")
	}
	want := "hotbed build 9 for " + runtime.GOOS + "/" + runtime.GOARCH
	if got, _ := os.ReadFile(exe); string(got) != want {
		t.Errorf("the controller's binary is %q, want %q", got, want)
	}
}

func mustRelease(t *testing.T, g *update.GitHub, n int64) update.Release {
	t.Helper()
	rel, err := g.Release(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}
