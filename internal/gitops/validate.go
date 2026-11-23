package gitops

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateSHA verifies that s is a strict 40-character (SHA-1) or 64-character
// (SHA-256) hexadecimal object ID.
func ValidateSHA(s string) error {
	if len(s) != 40 && len(s) != 64 {
		return fmt.Errorf("invalid commit SHA %q: must be 40 or 64 hex characters", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return fmt.Errorf("invalid commit SHA %q: must be hexadecimal", s)
		}
	}
	return nil
}

// ValidateRepoURL validates a remote repository URL for production use.
// It allows only https://, http://, ssh://, and SCP-style [user@]host:path URLs,
// and rejects empty strings, whitespace/control/NUL bytes, leading dashes,
// git remote helpers (ext::, fd::, remote-ext::, etc.), and local paths or
// file:// URLs.
func ValidateRepoURL(raw string) error {
	return validateRepoURL(raw, false)
}

// ValidateRepoURLAllowLocal is like ValidateRepoURL, but additionally permits
// file:// URLs and absolute local filesystem paths (for test harnesses).
// Leading dashes, whitespace/control/NUL bytes, and git remote helpers such as
// ext::, fd::, and remote-ext:: are still rejected.
func ValidateRepoURLAllowLocal(raw string) error {
	return validateRepoURL(raw, true)
}

func validateRepoURL(raw string, allowLocal bool) error {
	if raw == "" {
		return fmt.Errorf("repo URL must not be empty")
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("repo URL contains invalid UTF-8")
	}
	for _, r := range raw {
		if r == 0 || r < 0x20 || r == 0x7f || unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("repo URL must not contain whitespace or control characters")
		}
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("repo URL %q must not start with '-'", raw)
	}
	lower := strings.ToLower(raw)
	for _, helper := range []string{"ext::", "fd::", "remote-ext::"} {
		if strings.HasPrefix(lower, helper) {
			return fmt.Errorf("repo URL %q uses disallowed git remote helper", raw)
		}
	}
	if scheme, _, hasScheme := strings.Cut(raw, "://"); hasScheme {
		switch strings.ToLower(scheme) {
		case "https", "http", "ssh":
			return validateStandardRepoURL(raw)
		case "file":
			if !allowLocal {
				return fmt.Errorf("repo URL %q uses disallowed file:// scheme", raw)
			}
			return validateFileRepoURL(raw)
		default:
			return fmt.Errorf("repo URL %q uses unsupported scheme %q (allowed: https, http, ssh, or SCP-style [user@]host:path)", raw, scheme)
		}
	}
	if strings.HasPrefix(raw, "/") {
		if !allowLocal {
			return fmt.Errorf("repo URL %q is a local path, which is not allowed", raw)
		}
		if len(raw) <= 1 {
			return fmt.Errorf("repo URL %q is not a valid local repo path", raw)
		}
		return nil
	}
	if strings.HasPrefix(raw, ".") {
		return fmt.Errorf("repo URL %q is a relative local path, which is not allowed", raw)
	}
	return validateSCPRepoURL(raw)
}

func validateStandardRepoURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return fmt.Errorf("invalid repo URL %q", raw)
	}
	if u.User != nil {
		if user := u.User.Username(); strings.HasPrefix(user, "-") {
			return fmt.Errorf("repo URL %q username must not start with '-'", raw)
		}
	}
	host := u.Hostname()
	if u.Host == "" || host == "" {
		return fmt.Errorf("repo URL %q is missing a host", raw)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("repo URL %q host must not start with '-'", raw)
	}
	if !validHost(host) {
		return fmt.Errorf("repo URL %q has invalid host %q", raw, host)
	}
	if portStr := u.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("repo URL %q has invalid port %q", raw, portStr)
		}
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("repo URL %q is missing a repository path", raw)
	}
	return nil
}

func validateFileRepoURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return fmt.Errorf("invalid file repo URL %q", raw)
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return fmt.Errorf("file repo URL %q must not specify remote host %q", raw, u.Host)
	}
	if !strings.HasPrefix(u.Path, "/") || len(u.Path) <= 1 {
		return fmt.Errorf("file repo URL %q must have an absolute path", raw)
	}
	return nil
}

var disallowedSCPSchemes = map[string]struct{}{
	"file":       {},
	"ext":        {},
	"fd":         {},
	"remote-ext": {},
	"http":       {},
	"https":      {},
	"git":        {},
	"ftp":        {},
	"ftps":       {},
	"ssh":        {},
}

func validateSCPRepoURL(raw string) error {
	rest := raw
	var hasUser bool
	colonIdx := strings.IndexByte(raw, ':')
	bracketIdx := strings.IndexByte(raw, '[')
	atIdx := strings.IndexByte(raw, '@')
	if atIdx >= 0 && (colonIdx < 0 || atIdx < colonIdx) && (bracketIdx < 0 || atIdx < bracketIdx) {
		hasUser = true
		user := raw[:atIdx]
		rest = raw[atIdx+1:]
		if user == "" || rest == "" {
			return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or SCP-style [user@]host:path", raw)
		}
		if strings.HasPrefix(user, "-") {
			return fmt.Errorf("repo URL %q username must not start with '-'", raw)
		}
		for i := 0; i < len(user); i++ {
			c := user[i]
			valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '+' || c == '-'
			if !valid {
				return fmt.Errorf("repo URL %q has invalid SSH username %q", raw, user)
			}
		}
	}

	var host, pathPart string
	if strings.HasPrefix(rest, "[") {
		closeIdx := strings.Index(rest, "]:")
		if closeIdx < 0 {
			return fmt.Errorf("repo URL %q has invalid bracketed host", raw)
		}
		host = rest[1:closeIdx]
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Zone() != "" {
			return fmt.Errorf("repo URL %q has invalid IP literal %q", raw, host)
		}
		pathPart = rest[closeIdx+2:]
	} else {
		var hasColon bool
		host, pathPart, hasColon = strings.Cut(rest, ":")
		if !hasColon {
			return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or SCP-style [user@]host:path", raw)
		}
		if strings.HasPrefix(pathPart, ":") {
			return fmt.Errorf("repo URL %q uses disallowed git remote helper syntax", raw)
		}
		if !validHost(host) {
			return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or SCP-style [user@]host:path", raw)
		}
		if !hasUser {
			if len(host) == 1 && ((host[0] >= 'a' && host[0] <= 'z') || (host[0] >= 'A' && host[0] <= 'Z')) {
				return fmt.Errorf("repo URL %q looks like a drive-letter path, which is not allowed", raw)
			}
			if _, disallowed := disallowedSCPSchemes[strings.ToLower(host)]; disallowed {
				return fmt.Errorf("repo URL %q uses disallowed scheme prefix %q", raw, host)
			}
		}
	}
	if strings.HasPrefix(pathPart, ":") {
		return fmt.Errorf("repo URL %q uses disallowed git remote helper syntax", raw)
	}
	if pathPart == "" || strings.Trim(pathPart, "/") == "" {
		return fmt.Errorf("repo URL %q is missing a repository path", raw)
	}
	if strings.HasPrefix(pathPart, "-") {
		return fmt.Errorf("repo URL %q path must not start with '-'", raw)
	}
	return nil
}

func validHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == ""
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-'
		if !valid {
			return false
		}
	}
	return true
}

func validateBundlePath(path string) error {
	if path == "" {
		return fmt.Errorf("bundle path must not be empty")
	}
	if strings.HasPrefix(path, "-") {
		return fmt.Errorf("bundle path %q must not start with '-'", path)
	}
	for _, r := range path {
		if r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("bundle path %q contains control characters", path)
		}
	}
	return nil
}
