package macworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// Release is a cladefs release's machined for macOS on Apple silicon: what
// the daemon runs. A new one is pinned here, by SHA-256, as cladefs
// publishes it (its release workflow's machined-macos job).
type Release struct {
	// Tag is the release's (cladefs-<sha8>).
	Tag string
	// Machined and Helper are machined and machined-vm, the helper process
	// each machine's VM runs in: machined runs the one beside it.
	Machined, Helper Asset
}

// Asset is one file of a release.
type Asset struct {
	Name   string
	SHA256 string
}

// URL is where a's file of r downloads from.
func (r Release) URL(a Asset) string {
	return "https://github.com/semistrict/cladefs/releases/download/" + r.Tag + "/" + a.Name
}

// Ensure downloads r's binaries, if they are not there yet, into a
// directory of their own under p.Machined, checking each against its
// SHA-256, and returns machined's path (machined-vm beside it).
func (r Release) Ensure(ctx context.Context, p Paths, hc *http.Client) (string, error) {
	dir := filepath.Join(p.Machined, "bin", r.Tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, f := range []struct {
		asset Asset
		name  string
	}{{r.Machined, "machined"}, {r.Helper, "machined-vm"}} {
		if err := r.fetch(ctx, hc, f.asset, filepath.Join(dir, f.name)); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "machined"), nil
}

// fetch puts a at dst unless a file of a's SHA-256 is there already.
func (r Release) fetch(ctx context.Context, hc *http.Client, a Asset, dst string) error {
	if sum, err := fileSHA256(dst); err == nil && sum == a.SHA256 {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	url := r.URL(a)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download machined's %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download machined's %s: %s: %s", a.Name, url, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download machined's %s: %w", a.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("machined's %s from %s has SHA-256 %s, want %s", a.Name, url, got, a.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
