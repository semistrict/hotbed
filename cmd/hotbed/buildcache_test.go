package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/update/updatetest"
)

// buildCache is a directory of builds as CI publishes them: Dir/N holds
// build N's binary per platform and their SHA256SUMS.
type buildCache struct{ Dir string }

// hostBuild is build n's binary for this machine, as publishBuild writes it.
func hostBuild(n int64) string {
	here := update.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	return string(updatetest.BinaryFor(here, fmt.Sprintf("hotbed build %d for %s", n, here)))
}

// publishBuild writes build n into a new cache, and returns the cache and
// the build's files by name.
func publishBuild(t *testing.T, n int64) (*buildCache, map[string][]byte) {
	t.Helper()
	cache := &buildCache{Dir: t.TempDir()}
	dir := filepath.Join(cache.Dir, fmt.Sprint(n))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	var sums bytes.Buffer
	for _, p := range update.Platforms {
		b := updatetest.BinaryFor(p, fmt.Sprintf("hotbed build %d for %s", n, p))
		files[p.Asset()] = b
		fmt.Fprintf(&sums, "%s  %s\n", updatetest.Sum(b), p.Asset())
	}
	files[update.SumsName] = sums.Bytes()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cache, files
}

// releaseDownloads serves a build's files as GitHub's public release
// downloads do, at /semistrict/hotbed/releases/download/build-N/NAME, and
// refuses a request that carries a token.
type releaseDownloads struct {
	*httptest.Server
	Hits atomic.Int32
}

func serveReleaseDownloads(t *testing.T, n int64, files map[string][]byte) *releaseDownloads {
	t.Helper()
	d := &releaseDownloads{}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("GET /semistrict/hotbed/releases/download/build-%d/{name}", n), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "public release download must not receive Authorization header", http.StatusBadRequest)
			return
		}
		b, ok := files[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.Hits.Add(1)
		w.Write(b)
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}
