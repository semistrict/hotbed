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
	"time"
)

// Dir is where `pnpm -C cf-controller run build` writes the bundle, relative
// to the repo root.
var Dir = filepath.Join("cf-controller", ".cloudflare", "output", "v1", "workers", "default", "bundle")

// sources are what the bundle is built from, relative to cf-controller.
var sources = []string{"src", "drizzle", "package.json", "cloudflare.config.ts"}

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

func ensure(root string, build func(controller string) error) (string, error) {
	dir := filepath.Join(root, Dir)
	controller := filepath.Join(root, "cf-controller")
	built, err := modTime(filepath.Join(dir, "index.js"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	stale := err != nil
	if !stale {
		newest, err := newestSource(controller)
		if err != nil {
			return "", err
		}
		stale = newest.After(built)
	}
	if stale {
		if err := build(controller); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func modTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// newestSource is the latest modification time among the bundle's sources
// under controller, the cf-controller directory.
func newestSource(controller string) (time.Time, error) {
	var newest time.Time
	for _, src := range sources {
		err := filepath.WalkDir(filepath.Join(controller, src), func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
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
	}
	return newest, nil
}
