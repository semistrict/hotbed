// Package update moves hotbed's nodes to new builds. CI publishes each
// build of main as a GitHub release, build-N, with a binary per platform and
// their sums. The controller alone reaches GitHub: it fetches the newest
// build into a cache in its state directory, installs it over itself, and
// serves the cache to its workers and clients, which install the
// controller's build over themselves. Every node so runs the controller's
// build.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/hotbed/internal/version"
)

// ErrRestart is what a node's serve returns once it installed a new build
// over itself: its service manager starts it again, as the new build.
var ErrRestart = errors.New("installed a new build: restart")

// Platform is an OS and an architecture CI builds for.
type Platform struct{ OS, Arch string }

// Platforms are what CI builds: the workers' Linux and the Macs clients run
// on.
var Platforms = []Platform{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Asset is the platform's binary's name in a release and in the cache.
func (p Platform) Asset() string { return "hotbed-" + p.OS + "-" + p.Arch }

// SumsName is a release's sums, in sha256sum's format.
const SumsName = "SHA256SUMS"

// GitHub reads the releases of a repo, as its token allows.
type GitHub struct {
	API    string // https://api.github.com, or a test's
	Repo   string // owner/name
	Token  string
	Client *http.Client
}

// Release is a build CI published: its assets' API URLs by name.
type Release struct {
	Build  int64
	Assets map[string]string
}

// Latest is the newest build.
func (g *GitHub) Latest(ctx context.Context) (Release, error) {
	return g.release(ctx, "latest")
}

// Release is build n.
func (g *GitHub) Release(ctx context.Context, n int64) (Release, error) {
	return g.release(ctx, "tags/"+version.Tag(n))
}

func (g *GitHub) release(ctx context.Context, which string) (Release, error) {
	resp, err := g.get(ctx, g.API+"/repos/"+g.Repo+"/releases/"+which, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	var body struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("read release %s of %s: %w", which, g.Repo, err)
	}
	rel := Release{Build: version.ParseTag(body.Tag), Assets: map[string]string{}}
	if rel.Build == 0 {
		return Release{}, fmt.Errorf("release %s of %s is tagged %q, not build-N", which, g.Repo, body.Tag)
	}
	for _, a := range body.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// Asset opens one of a release's assets.
func (g *GitHub) Asset(ctx context.Context, rel Release, name string) (io.ReadCloser, error) {
	url, ok := rel.Assets[name]
	if !ok {
		return nil, fmt.Errorf("build %d has no %s", rel.Build, name)
	}
	resp, err := g.get(ctx, url, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (g *GitHub) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := *g.Client
	// An asset redirects to storage, which takes a signed URL, not the token.
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		r.Header.Del("Authorization")
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Cache keeps builds: Dir/N holds build N's binaries and their sums, whole
// or not at all.
type Cache struct{ Dir string }

func (c Cache) dir(n int64) string { return filepath.Join(c.Dir, strconv.FormatInt(n, 10)) }

// Has says whether build n is in the cache.
func (c Cache) Has(n int64) bool {
	_, err := os.Stat(filepath.Join(c.dir(n), SumsName))
	return err == nil
}

// Put fetches a release's binaries into the cache, each checked against the
// release's sums; a build that fails leaves nothing.
func (c Cache) Put(ctx context.Context, g *GitHub, rel Release) (err error) {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(c.Dir, ".fetch-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	sums, err := fetch(ctx, g, rel, SumsName, filepath.Join(tmp, SumsName))
	if err != nil {
		return err
	}
	want, err := parseSums(sums)
	if err != nil {
		return fmt.Errorf("build %d's %s: %w", rel.Build, SumsName, err)
	}
	for _, p := range Platforms {
		b, err := fetch(ctx, g, rel, p.Asset(), filepath.Join(tmp, p.Asset()))
		if err != nil {
			return err
		}
		if got := hash(b); got != want[p.Asset()] {
			return fmt.Errorf("build %d's %s: sha256 %s, its sums say %q", rel.Build, p.Asset(), got, want[p.Asset()])
		}
	}
	return os.Rename(tmp, c.dir(rel.Build))
}

func fetch(ctx context.Context, g *GitHub, rel Release, name, path string) ([]byte, error) {
	r, err := g.Asset(ctx, rel, name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("fetch build %d's %s: %w", rel.Build, name, err)
	}
	return b, os.WriteFile(path, b, 0o755)
}

func parseSums(b []byte) (map[string]string, error) {
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		s, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			return nil, fmt.Errorf("line %q is not SUM  NAME", sc.Text())
		}
		m[name] = s
	}
	return m, sc.Err()
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Binary is where build n's binary for p is in the cache, and its sum.
func (c Cache) Binary(n int64, p Platform) (path, sum string, err error) {
	b, err := os.ReadFile(filepath.Join(c.dir(n), SumsName))
	if err != nil {
		return "", "", err
	}
	sums, err := parseSums(b)
	if err != nil {
		return "", "", err
	}
	s, ok := sums[p.Asset()]
	if !ok {
		return "", "", fmt.Errorf("build %d has no binary for %s: %w", n, p, fs.ErrNotExist)
	}
	return filepath.Join(c.dir(n), p.Asset()), s, nil
}

// Prune removes every build but keep, and what a fetch left half done.
func (c Cache) Prune(keep ...int64) {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n, err := strconv.ParseInt(e.Name(), 10, 64)
		if err == nil && slices.Contains(keep, n) {
			continue
		}
		os.RemoveAll(filepath.Join(c.Dir, e.Name()))
	}
}

// Install puts the binary r reads in place of exe, in one rename, once it
// matches sum.
func Install(r io.Reader, sum, exe string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".new-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the new binary's sha256 is %s, not %s", got, sum)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}
