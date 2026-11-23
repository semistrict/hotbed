package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestNewHandlerSecurityHeaders(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><html><body>cockpit</body></html>")},
		"assets/app-123.js": {Data: []byte("console.log('ok');")},
		"favicon.svg":       {Data: []byte("<svg></svg>")},
	}
	h := NewHandler(fsys)

	wantHeaders := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": cockpitCSP,
	}

	cases := []struct {
		name         string
		path         string
		wantStatus   int
		wantCacheCtl string
	}{
		{name: "root index", path: "/", wantStatus: http.StatusOK, wantCacheCtl: "no-cache"},
		{name: "spa fallback route", path: "/sessions/tidy-owl", wantStatus: http.StatusOK, wantCacheCtl: "no-cache"},
		{name: "hashed static asset", path: "/assets/app-123.js", wantStatus: http.StatusOK, wantCacheCtl: "public, max-age=31536000, immutable"},
		{name: "root static file", path: "/favicon.svg", wantStatus: http.StatusOK},
		{name: "missing asset 404", path: "/assets/missing.js", wantStatus: http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", tc.path, rec.Code, tc.wantStatus)
			}
			for k, want := range wantHeaders {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("GET %s header %s = %q, want %q", tc.path, k, got, want)
				}
			}
			if tc.wantCacheCtl != "" {
				if got := rec.Header().Get("Cache-Control"); got != tc.wantCacheCtl {
					t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.wantCacheCtl)
				}
			}
		})
	}

	t.Run("unbuilt ui still sets security headers", func(t *testing.T) {
		unbuilt := NewHandler(fstest.MapFS{})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		unbuilt.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		for k, want := range wantHeaders {
			if got := rec.Header().Get(k); got != want {
				t.Errorf("header %s = %q, want %q", k, got, want)
			}
		}
	})
}
