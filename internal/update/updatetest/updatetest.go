// Package updatetest stands in for GitHub's releases API, for tests of
// builds and updates.
package updatetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/update"
)

// GitHub serves releases as GitHub's API does: a release's assets by
// API URL, each fetched with Accept: application/octet-stream, by a
// redirect to storage that must not see the token.
type GitHub struct {
	srv      *httptest.Server
	token    string
	Releases map[int64]map[string][]byte // build -> asset name -> content
	latest   int64
}

func NewGitHub(t *testing.T) *GitHub {
	g := &GitHub{token: "secret", Releases: map[int64]map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/semistrict/hotbed/releases", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorized(w, r) {
			return
		}
		var nums []int64
		for n := range g.Releases {
			nums = append(nums, n)
		}
		slices.SortFunc(nums, func(a, b int64) int {
			if a > b {
				return -1
			}
			if a < b {
				return 1
			}
			return 0
		})
		var items []string
		for _, n := range nums {
			items = append(items, g.releaseJSON(n))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/semistrict/hotbed/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		g.release(w, r, g.latest)
	})
	mux.HandleFunc("GET /repos/semistrict/hotbed/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		fmt.Sscanf(r.PathValue("tag"), "build-%d", &n)
		g.release(w, r, n)
	})
	mux.HandleFunc("GET /repos/semistrict/hotbed/releases/assets/{build}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorized(w, r) {
			return
		}
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want octet-stream", http.StatusUnsupportedMediaType)
			return
		}
		http.Redirect(w, r, "/storage/"+r.PathValue("build")+"/"+r.PathValue("name"), http.StatusFound)
	})
	mux.HandleFunc("GET /storage/{build}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "storage takes no token", http.StatusBadRequest)
			return
		}
		var n int64
		fmt.Sscan(r.PathValue("build"), &n)
		b, ok := g.Releases[n][r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *GitHub) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return false
	}
	return true
}

func (g *GitHub) releaseJSON(n int64) string {
	assets := g.Releases[n]
	var list []string
	for name := range assets {
		list = append(list, fmt.Sprintf(`{"name":%q,"url":"%s/repos/semistrict/hotbed/releases/assets/%d/%s"}`, name, g.srv.URL, n, name))
	}
	return fmt.Sprintf(`{"tag_name":"build-%d","assets":[%s]}`, n, strings.Join(list, ","))
}

func (g *GitHub) release(w http.ResponseWriter, r *http.Request, n int64) {
	if !g.authorized(w, r) {
		return
	}
	if _, ok := g.Releases[n]; !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	fmt.Fprint(w, g.releaseJSON(n))
}

// Publish adds build n, as CI publishes it: a binary per platform and their
// sums.
func (g *GitHub) Publish(n int64) {
	assets := map[string][]byte{}
	var sums bytes.Buffer
	for _, p := range update.Platforms {
		b := []byte(fmt.Sprintf("hotbed build %d for %s", n, p))
		assets[p.Asset()] = b
		fmt.Fprintf(&sums, "%s  %s\n", Sum(b), p.Asset())
	}
	assets[update.SumsName] = sums.Bytes()
	g.Releases[n] = assets
	g.latest = max(g.latest, n)
}

// Sum is b's sha256, as SHA256SUMS has it.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Client reads the fake's releases.
func (g *GitHub) Client() *update.GitHub {
	return &update.GitHub{API: g.srv.URL, Repo: "semistrict/hotbed", Token: g.token, Client: g.srv.Client()}
}
