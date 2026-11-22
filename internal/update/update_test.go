package update_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

func newFakeGitHub(t *testing.T) *updatetest.GitHub { return updatetest.NewGitHub(t) }

func sum(b []byte) string { return updatetest.Sum(b) }

func TestLatestAndFetchIntoTheCache(t *testing.T) {
	g := newFakeGitHub(t)
	g.Publish(41)
	g.Publish(42)
	gh := g.Client()
	rel, err := gh.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Build != 42 {
		t.Fatalf("latest build = %d, want 42", rel.Build)
	}
	cache := update.Cache{Dir: t.TempDir()}
	if cache.Has(42) {
		t.Fatal("an empty cache has build 42")
	}
	if err := cache.Put(t.Context(), gh, rel); err != nil {
		t.Fatal(err)
	}
	if !cache.Has(42) {
		t.Fatal("the cache doesn't have build 42 after Put")
	}
	for _, p := range update.Platforms {
		path, s, err := cache.Binary(42, p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "hotbed build 42 for "+p.String() || s != sum(b) {
			t.Errorf("%s: %q, sum %s", p, b, s)
		}
	}
	old, err := gh.Release(t.Context(), 41)
	if err != nil || old.Build != 41 {
		t.Fatalf("Release(41) = %+v, %v", old, err)
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

func TestLatestWithoutTheTokenFails(t *testing.T) {
	g := newFakeGitHub(t)
	g.Publish(42)
	gh := g.Client()
	gh.Token = "wrong"
	if _, err := gh.Latest(t.Context()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Latest with a wrong token = %v, want GitHub's 404", err)
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

func TestPrune(t *testing.T) {
	g := newFakeGitHub(t)
	gh := g.Client()
	cache := update.Cache{Dir: t.TempDir()}
	for _, n := range []int64{40, 41, 42} {
		g.Publish(n)
		rel, err := gh.Release(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.Put(t.Context(), gh, rel); err != nil {
			t.Fatal(err)
		}
	}
	cache.Prune(42, 40)
	if !cache.Has(42) || cache.Has(41) || !cache.Has(40) {
		t.Errorf("after Prune(42, 40): has 40 %v, 41 %v, 42 %v", cache.Has(40), cache.Has(41), cache.Has(42))
	}
}
