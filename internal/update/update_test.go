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
