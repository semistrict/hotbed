// Package ui serves hotbed's web cockpit: the single-page app in this
// directory, built by `pnpm build` into dist/ and compiled into the binary.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:generate pnpm install --frozen-lockfile
//go:generate pnpm build

//go:embed all:dist
var dist embed.FS

// Handler serves the built UI from the binary.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// dist is embedded at compile time, so this cannot fail at run time.
		panic(err)
	}
	return NewHandler(sub)
}

const cockpitCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", cockpitCSP)
}

// NewHandler serves the single-page app in fsys. A path that names a file
// gets the file; /assets/ paths that do not exist are 404s; any other path
// gets index.html, so the app's router can handle it.
func NewHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	_, err := fs.Stat(fsys, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if !built {
			http.Error(w, "this hotbed was built without its UI: run `go generate ./ui` and build again", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if info, err := fs.Stat(fsys, name); err == nil && !info.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite puts a content hash in every asset's name.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}
