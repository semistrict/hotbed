package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// ArtifactStore caches controller-distributed binaries and guest kernels
// (hotbed-guestd, vmlinux) under <WorkerDir>/artifacts/<kind>/<arch>/<sha16>/<filename>,
// verified by SHA-256 and placed atomically so running VMs are never disrupted.
type ArtifactStore struct {
	WorkerDir      string
	ControllerURL  string
	AuthClient     *http.Client // authenticated client for controller-relative paths (/builds/...)
	DownloadClient *http.Client // clean client (no controller auth) for direct URLs

	mu sync.Mutex
}

type artifactKind struct {
	dir      string
	filename string
	mode     os.FileMode
}

var (
	artifactGuestd = artifactKind{dir: "guestd", filename: "hotbed-guestd", mode: 0o755}
	artifactKernel = artifactKind{dir: "kernels", filename: "vmlinux", mode: 0o644}
)

// CachedPath returns the on-disk path if art is already present and valid in
// the local artifact store (without performing any network I/O).
func (s *ArtifactStore) CachedPath(kind artifactKind, arch string, art *pb.ManagedArtifact) (string, bool) {
	if s == nil || s.WorkerDir == "" || art == nil {
		return "", false
	}
	sha := strings.ToLower(strings.TrimSpace(art.GetSha256()))
	if len(sha) != 64 {
		return "", false
	}
	p := s.targetPath(kind, arch, sha)
	if ok, _ := fileMatchesSHA256(p, sha); ok {
		return p, true
	}
	return "", false
}

// Ensure downloads, verifies, and atomically installs art into the content-
// addressed artifact cache if not already present, returning its local path.
func (s *ArtifactStore) Ensure(ctx context.Context, kind artifactKind, arch string, art *pb.ManagedArtifact) (string, error) {
	if s == nil || s.WorkerDir == "" {
		return "", fmt.Errorf("artifact store has no worker dir")
	}
	if art == nil {
		return "", fmt.Errorf("artifact is nil")
	}
	sha := strings.ToLower(strings.TrimSpace(art.GetSha256()))
	if len(sha) != 64 {
		return "", fmt.Errorf("%s artifact for %s has invalid sha256 %q", kind.dir, arch, art.GetSha256())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dst := s.targetPath(kind, arch, sha)
	if ok, _ := fileMatchesSHA256(dst, sha); ok {
		_ = os.Chmod(dst, kind.mode)
		return dst, nil
	}

	downloadURL, client, err := s.resolveSource(art)
	if err != nil {
		return "", fmt.Errorf("resolve %s artifact source: %w", kind.dir, err)
	}

	// Ensure parent directories are traversable by non-root Firecracker VMUser (e.g. cladefsd).
	_ = ensureDirTraversable(s.WorkerDir)
	for _, d := range []string{
		filepath.Join(s.WorkerDir, "artifacts"),
		filepath.Join(s.WorkerDir, "artifacts", kind.dir),
		filepath.Join(s.WorkerDir, "artifacts", kind.dir, arch),
		filepath.Dir(dst),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
		_ = os.Chmod(d, 0o755)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hotbed-worker")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", downloadURL, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+kind.filename+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download %s: %w", downloadURL, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != sha {
		return "", fmt.Errorf("%s artifact sha256 mismatch: got %s, want %s", kind.dir, got, sha)
	}
	if err := os.Chmod(tmpPath, kind.mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", err
	}
	ok = true

	// Prune older versions beyond the 2 newest (current + previous) so active
	// VMs are not disturbed while disk usage stays bounded.
	s.pruneOldLocked(filepath.Join(s.WorkerDir, "artifacts", kind.dir, arch), filepath.Base(filepath.Dir(dst)), 2)
	return dst, nil
}

func (s *ArtifactStore) targetPath(kind artifactKind, arch, sha string) string {
	return filepath.Join(s.WorkerDir, "artifacts", kind.dir, arch, sha[:16], kind.filename)
}

func (s *ArtifactStore) resolveSource(art *pb.ManagedArtifact) (string, *http.Client, error) {
	if raw := strings.TrimSpace(art.GetUrl()); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", nil, fmt.Errorf("invalid url %q", raw)
		}
		c := s.DownloadClient
		if c == nil {
			c = &http.Client{Timeout: 10 * time.Minute}
		}
		return raw, c, nil
	}
	if p := strings.TrimSpace(art.GetPath()); p != "" {
		if s.ControllerURL == "" {
			return "", nil, fmt.Errorf("artifact path %q has no controller URL", p)
		}
		c := s.AuthClient
		if c == nil {
			c = s.DownloadClient
		}
		if c == nil {
			c = &http.Client{Timeout: 10 * time.Minute}
		}
		return strings.TrimRight(s.ControllerURL, "/") + p, c, nil
	}
	return "", nil, fmt.Errorf("artifact has neither url nor path")
}

func (s *ArtifactStore) pruneOldLocked(archDir, keepDir string, keepCount int) {
	ents, err := os.ReadDir(archDir)
	if err != nil || len(ents) <= keepCount {
		return
	}
	type entry struct {
		name  string
		mtime time.Time
	}
	var dirs []entry
	for _, ent := range ents {
		if !ent.IsDir() || ent.Name() == keepDir {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, entry{name: ent.Name(), mtime: info.ModTime()})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].mtime.After(dirs[j].mtime) })
	// Keep (keepCount - 1) older dirs alongside keepDir.
	for i := max(keepCount-1, 0); i < len(dirs); i++ {
		_ = os.RemoveAll(filepath.Join(archDir, dirs[i].name))
	}
}

func fileMatchesSHA256(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(strings.TrimSpace(want)), nil
}

// syncFileAtomically copies src to dst atomically if dst is missing or has a
// different SHA-256. Used to keep default system paths like /usr/local/bin/hotbed-guestd
// aligned with the managed artifact cache.
func syncFileAtomically(src, dst, wantSHA string, mode os.FileMode) error {
	if ok, _ := fileMatchesSHA256(dst, wantSHA); ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// ensureDirTraversable adds o+x/g+x (0111) to dir without granting read/list
// permissions, so non-root Firecracker VMUser can traverse workerDir to reach
// world-readable subdirectories like vms/ and artifacts/.
func ensureDirTraversable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	perm := info.Mode().Perm()
	want := perm | 0o111
	if want == perm {
		return nil
	}
	return os.Chmod(dir, want)
}
