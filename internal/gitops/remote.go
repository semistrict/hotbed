package gitops

import (
	"net/url"
	"path/filepath"
	"strings"
)

// CanonicalRemote canonicalizes HTTPS, SSH, SCP-style, and local git URLs so
// "git@github.com:semistrict/hotbed.git" and "https://github.com/semistrict/hotbed"
// compare equal.
func CanonicalRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Local file paths (non-URL, non-SCP).
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		s = strings.TrimSuffix(filepath.Clean(s), ".git")
		return CanonicalPath(s)
	}
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err == nil {
			p := strings.TrimSuffix(filepath.Clean(u.Path), ".git")
			return CanonicalPath(p)
		}
	}
	// SCP-style: [user@]host:path/to/repo.git
	if !strings.Contains(s, "://") {
		if hostPart, pathPart, ok := strings.Cut(s, ":"); ok && !strings.Contains(hostPart, "/") {
			if _, host, hasUser := strings.Cut(hostPart, "@"); hasUser {
				hostPart = host
			}
			pathPart = strings.Trim(pathPart, "/")
			pathPart = strings.TrimSuffix(pathPart, ".git")
			return strings.ToLower(hostPart) + "/" + pathPart
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	}
	p := strings.Trim(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.ToLower(u.Hostname()) + "/" + p
}

// CanonicalPath is p absolute, its symlinks resolved where they can be.
func CanonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}
