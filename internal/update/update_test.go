package update_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

func newFakeGitHub(t *testing.T) *updatetest.GitHub { return updatetest.NewGitHub(t) }

func sum(b []byte) string { return updatetest.Sum(b) }

// Put streams each platform binary directly to disk while hashing, verifies
// every sum, marks binaries executable, and leaves no temporary files behind.
func TestPutStreamsReleaseToCache(t *testing.T) {
	g := newFakeGitHub(t)
	g.Publish(41)
	// Replace one binary with a multi-chunk payload and update SHA256SUMS so we
	// exercise multi-read streaming through io.MultiWriter.
	big := bytes.Repeat([]byte("hotbed-binary-chunk-"), 16384) // 320 KiB
	g.Releases[41][update.Platforms[0].Asset()] = big
	var sums strings.Builder
	for _, p := range update.Platforms {
		sums.WriteString(sum(g.Releases[41][p.Asset()]) + "  " + p.Asset() + "\n")
	}
	g.Releases[41][update.SumsName] = []byte(sums.String())

	gh := g.Client()
	rel, err := gh.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cache := update.Cache{Dir: t.TempDir()}
	if err := cache.Put(t.Context(), gh, rel); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !cache.Has(41) {
		t.Fatal("cache does not report build 41 after Put")
	}
	for _, p := range update.Platforms {
		path, gotSum, err := cache.Binary(41, p)
		if err != nil {
			t.Fatalf("Binary(%s): %v", p, err)
		}
		wantBytes := g.Releases[41][p.Asset()]
		if gotSum != sum(wantBytes) {
			t.Errorf("Binary(%s) sum = %s, want %s", p, gotSum, sum(wantBytes))
		}
		gotBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read cached %s: %v", p, err)
		}
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Errorf("cached %s content mismatch (%d vs %d bytes)", p, len(gotBytes), len(wantBytes))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("cached %s mode = %v, want 0755", p, info.Mode().Perm())
		}
	}
	// Ensure no leftover .fetch-* or .part-* files remain in cache or build dir.
	if entries, err := os.ReadDir(cache.Dir); err != nil || len(entries) != 1 || entries[0].Name() != "41" {
		t.Errorf("cache dir entries = %v (err=%v), want only [41]", entries, err)
	}
	buildEntries, err := os.ReadDir(filepath.Join(cache.Dir, "41"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range buildEntries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temp file in build dir: %s", e.Name())
		}
	}
}

// A build whose binary doesn't match its sums is refused, and nothing of
// it is kept.
func TestPutRefusesABadSum(t *testing.T) {
	g := newFakeGitHub(t)
	g.Publish(42)
	g.Releases[42][update.Platforms[1].Asset()] = []byte("tampered")
	gh := g.Client()
	rel, err := gh.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cache := update.Cache{Dir: t.TempDir()}
	err = cache.Put(t.Context(), gh, rel)
	if err == nil || !strings.Contains(err.Error(), update.Platforms[1].Asset()+": sha256 ") {
		t.Fatalf("Put = %v, want a sum mismatch for %s", err, update.Platforms[1].Asset())
	}
	if cache.Has(42) {
		t.Error("the cache kept a build that failed its sums")
	}
	if entries, _ := os.ReadDir(cache.Dir); len(entries) != 0 {
		t.Errorf("the cache holds %v after a failed Put", entries)
	}
}

// Install replaces a binary in one rename, executable, and only with what
// matches its sum.
func TestInstall(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "hotbed")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := update.Install(strings.NewReader("new"), sum([]byte("other")), exe); err == nil {
		t.Fatal("Install took a binary whose sum doesn't match")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("a refused Install left %q", b)
	}
	if err := update.Install(strings.NewReader("new"), sum([]byte("new")), exe); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" || info.Mode().Perm() != 0o755 {
		t.Errorf("after Install: %q, mode %v", b, info.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("Install left %v beside the binary", entries)
	}
}

// A public repository needs no token: when Token is empty, GitHub omits the
// Authorization header rather than sending an empty Bearer header.
func TestGitHubOmitsAuthHeaderWithoutToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"tag_name":"build-7","assets":[]}`))
	}))
	t.Cleanup(srv.Close)

	gh := &update.GitHub{API: srv.URL, Repo: "semistrict/hotbed", Client: srv.Client()}
	rel, err := gh.Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest without token: %v", err)
	}
	if rel.Build != 7 {
		t.Errorf("Build = %d, want 7", rel.Build)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty", gotAuth)
	}
}
