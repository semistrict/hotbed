// Package cfbundle finds the cf-controller Worker bundle that local
// harnesses (the CLI tests' and dev-cockpit) run in workerd, building it
// first when it is missing or older than the controller's sources, so they
// never run a controller other than the one in the checkout.
package cfbundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Dir is where `pnpm -C cf-controller run build` writes the bundle, relative
// to the repo root.
var Dir = filepath.Join("cf-controller", ".cloudflare", "output", "v1", "workers", "default", "bundle")

// Ensure returns the bundle directory under repo root, building the bundle
// when it is missing or older than any of its sources.
func Ensure(root string) (string, error) {
	return ensure(root, func(controller string) error {
		build := exec.Command("pnpm", "-C", controller, "run", "build")
		if out, err := build.CombinedOutput(); err != nil {
			return fmt.Errorf("the cf-controller bundle is missing or older than its sources, and building it failed (install its dependencies first: pnpm -C cf-controller install --frozen-lockfile): %w\n%s", err, out)
		}
		return nil
	})
}

// lockDir holds the lock that serializes builds, under cf-controller: hidden,
// so newestSource skips it, and apart from .cloudflare, which a build clears.
const lockDir = ".cfbundle"

func ensure(root string, build func(controller string) error) (string, error) {
	dir := filepath.Join(root, Dir)
	controller := filepath.Join(root, "cf-controller")
	if stale, err := isStale(dir, controller); err != nil || !stale {
		return dir, err
	}
	// Harnesses run in parallel test binaries (go test ./... runs cmd/hotbed's
	// and dev-cockpit's at once), and a build clears the bundle before writing
	// it: one process's build must never run under another's harness starting
	// from the bundle. So one builds at a time, and each looks again once it
	// holds the lock, finding the bundle another process just built.
	err := withLock(filepath.Join(controller, lockDir), func() error {
		stale, err := isStale(dir, controller)
		if err != nil || !stale {
			return err
		}
		return build(controller)
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// isStale reports whether the bundle in dir is missing or older than any of
// controller's sources.
func isStale(dir, controller string) (bool, error) {
	built, err := modTime(filepath.Join(dir, "index.js"))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	newest, err := newestSource(controller)
	if err != nil {
		return false, err
	}
	return newest.After(built), nil
}

// withLock runs fn holding an exclusive lock on a file in dir, across
// processes.
func withLock(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make the cf-controller build lock's directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "build.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open the cf-controller build lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("take the cf-controller build lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func modTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// newestSource is the latest modification time of any file the bundle may
// be built from: all of controller, the cf-controller directory, but its
// dependencies (node_modules) and what tools write (hidden directories, such
// as .cloudflare, where the bundle itself is).
func newestSource(controller string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(controller, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != controller && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("look at cf-controller sources: %w", err)
	}
	return newest, nil
}
