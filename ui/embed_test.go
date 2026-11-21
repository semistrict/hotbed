package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

var app = fstest.MapFS{
	"index.html":         {Data: []byte("<!doctype html><div id=root></div>")},
	"favicon.svg":        {Data: []byte("<svg/>")},
	"assets/index-1.js":  {Data: []byte("console.log(1)")},
	"assets/index-1.css": {Data: []byte("body{}")},
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func body(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServesIndexForAppRoutes(t *testing.T) {
	h := NewHandler(app)
	for _, path := range []string{"/", "/nodes", "/workspaces/ws-3f2a", "/commands/c-1?state=parked"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK || body(t, rec) != "<!doctype html><div id=root></div>" {
			t.Errorf("GET %s = %d %q, want index.html", path, rec.Code, body(t, rec))
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

func TestIndexHTMLRedirectsToRoot(t *testing.T) {
	rec := get(t, NewHandler(app), "/index.html")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "./" {
		t.Errorf("GET /index.html = %d to %q, want 301 to ./", rec.Code, rec.Header().Get("Location"))
	}
}

func TestServesAssetsImmutable(t *testing.T) {
	rec := get(t, NewHandler(app), "/assets/index-1.js")
	if rec.Code != http.StatusOK || body(t, rec) != "console.log(1)" {
		t.Fatalf("GET asset = %d %q", rec.Code, body(t, rec))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestMissingAssetIs404(t *testing.T) {
	if rec := get(t, NewHandler(app), "/assets/gone-2.js"); rec.Code != http.StatusNotFound {
		t.Errorf("GET missing asset = %d, want 404", rec.Code)
	}
}

func TestUnbuiltUIExplainsItself(t *testing.T) {
	rec := get(t, NewHandler(fstest.MapFS{".gitkeep": {}}), "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET / without a build = %d, want 503", rec.Code)
	}
}
