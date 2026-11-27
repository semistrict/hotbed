package skillspec

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceType matches vercel-labs/skills source types.
type SourceType string

const (
	SourceLocal     SourceType = "local"
	SourceGitHub    SourceType = "github"
	SourceGitLab    SourceType = "gitlab"
	SourceGit       SourceType = "git"
	SourceWellKnown SourceType = "well-known"
	SourceDownload  SourceType = "download"
)

// ParsedSource is the parsed representation of an npx skills source argument.
type ParsedSource struct {
	Type          SourceType
	URL           string
	Subpath       string
	LocalPath     string
	Ref           string
	SkillFilter   string
	OriginalInput string
}

var sourceAliases = map[string]string{
	"coinbase/agentWallet":      "coinbase/agentic-wallet-skills",
	"vercel-labs/vercel-skills": "vercel-labs/agent-skills",
}

var (
	winAbsRe        = regexp.MustCompile(`^[a-zA-Z]:[/\\]`)
	commitSHARe     = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	versionPinRe    = regexp.MustCompile(`^v?\d+(?:\.\d+)+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	sshGitTreeRe    = regexp.MustCompile(`^git@([^:]+):([^/]+)/([^/]+?)(?:\.git)?/tree/([^/]+)(?:/(.+))?$`)
	ghTreeRe        = regexp.MustCompile(`^/([^/]+)/([^/]+)/tree/([^/]+)(?:/(.+?))?/?$`)
	ghRepoRe        = regexp.MustCompile(`^/([^/]+)/([^/]+)`)
	glTreePathRe    = regexp.MustCompile(`^(https?)://([^/]+)/(.+?)/-/tree/([^/]+)/(.+)$`)
	glTreeOnlyRe    = regexp.MustCompile(`^(https?)://([^/]+)/(.+?)/-/tree/([^/]+)$`)
	glRepoRe        = regexp.MustCompile(`^/(.+?)(?:\.git)?/?$`)
	atSkillRe       = regexp.MustCompile(`^([^/]+)/([^/@]+)@(.+)$`)
	shorthandRe     = regexp.MustCompile(`^([^/]+)/([^/]+)(?:/(.+?))?/?$`)
	azureVerRe      = regexp.MustCompile(`^(?i)(?:GB|GT)(.+)$`)
	mirrorHostRe    = regexp.MustCompile(`(?i)^(/(?:.*?/)?(?:https?://?)?(?:www\.)?(github|gitlab)\.com)(/.*)$`)
	mirrorGHRestRe  = regexp.MustCompile(`^(/[^/]+/[^/]+?)(?:\.git)?(?:/tree/([^/]+)(?:/(.+?))?)?/?$`)
	mirrorGLRestRe  = regexp.MustCompile(`^(/[^/]+(?:/[^/]+)*/[^/]+?)(?:\.git)?(?:/-/tree/([^/]+)(?:/(.+?))?)?/?$`)
	ghHostedArtRe   = regexp.MustCompile(`^/[^/]+/[^/]+/(?:archive/|raw/|releases/(?:download/|latest/download/))`)
	glHostedArtRe   = regexp.MustCompile(`/-/(:?archive|raw)/`)
	sshURLWithGitRe = regexp.MustCompile(`(?i)^ssh://.+\.git(?:$|[/?])`)
	httpWithGitRe   = regexp.MustCompile(`(?i)^https?://.+\.git(?:$|[/?])`)
)

// IsCommitSHA reports whether s is a full 40-character hexadecimal git commit SHA.
func IsCommitSHA(s string) bool { return commitSHARe.MatchString(s) }

func isVersionPin(s string) bool {
	return versionPinRe.MatchString(s) || IsCommitSHA(s)
}

func isLocalPath(input string) bool {
	return filepath.IsAbs(input) ||
		strings.HasPrefix(input, "./") ||
		strings.HasPrefix(input, "../") ||
		strings.HasPrefix(input, "~/") ||
		strings.HasPrefix(input, `~\`) ||
		input == "." ||
		input == ".." ||
		input == "~" ||
		winAbsRe.MatchString(input)
}

func resolveLocalPath(input, cwd string) (string, error) {
	resolved := input
	if resolved == "~" || strings.HasPrefix(resolved, "~/") || strings.HasPrefix(resolved, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for %q: %w", input, err)
		}
		if resolved == "~" {
			resolved = home
		} else {
			resolved = filepath.Join(home, resolved[2:])
		}
	}
	if !filepath.IsAbs(resolved) {
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		resolved = filepath.Join(cwd, resolved)
	}
	return filepath.Clean(resolved), nil
}

func hasExplicitRemotePrefix(input string) bool {
	lower := strings.ToLower(input)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "ssh://") ||
		strings.HasPrefix(lower, "git://") ||
		strings.HasPrefix(lower, "file://") ||
		strings.HasPrefix(lower, "github:") ||
		strings.HasPrefix(lower, "gitlab:") ||
		strings.HasPrefix(lower, "git@")
}

// SanitizeSubpath rejects path traversal ("..") segments in a source subpath.
func SanitizeSubpath(subpath string) (string, error) {
	norm := strings.ReplaceAll(subpath, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return "", fmt.Errorf("unsafe subpath %q: contains '..'", subpath)
		}
	}
	return subpath, nil
}

func gitHubHost(override string) string {
	if override != "" {
		return strings.ToLower(strings.TrimSpace(override))
	}
	if env := strings.TrimSpace(os.Getenv("GH_HOST")); env != "" {
		return strings.ToLower(env)
	}
	return "github.com"
}

func isGitHubHost(host, configuredHost string) bool {
	h := strings.ToLower(host)
	return h == "github.com" || h == "www.github.com" || (configuredHost != "" && h == configuredHost)
}

func pathOnHost(input, targetHost string) (string, bool) {
	rest := input
	if strings.HasPrefix(strings.ToLower(rest), "https://") {
		rest = rest[len("https://"):]
	} else if strings.HasPrefix(strings.ToLower(rest), "http://") {
		rest = rest[len("http://"):]
	}
	// Trim query/fragment
	if idx := strings.IndexAny(rest, "?#"); idx >= 0 {
		rest = rest[:idx]
	}
	hostPart, pathPart := rest, ""
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		hostPart, pathPart = rest[:idx], rest[idx:]
	}
	if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	if colon := strings.IndexByte(hostPart, ':'); colon >= 0 {
		hostPart = hostPart[:colon]
	}
	hostPart = strings.ToLower(hostPart)
	if hostPart == targetHost || hostPart == "www."+targetHost {
		return pathPart, true
	}
	return "", false
}

func looksLikeGitSource(input, ghHost string) bool {
	if strings.HasPrefix(input, "github:") || strings.HasPrefix(input, "gitlab:") || strings.HasPrefix(input, "git@") {
		return true
	}
	if sshURLWithGitRe.MatchString(input) || httpWithGitRe.MatchString(input) {
		return true
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		if u, err := url.Parse(input); err == nil {
			if isGitHubHost(u.Host, ghHost) {
				return regexp.MustCompile(`^/[^/]+/[^/]+(?:\.git)?(?:/tree/[^/]+(?:/.*)?)?/?$`).MatchString(u.Path)
			}
			if strings.EqualFold(u.Hostname(), "gitlab.com") {
				return regexp.MustCompile(`^/.+?/[^/]+(?:\.git)?(?:/-/tree/[^/]+(?:/.*)?)?/?$`).MatchString(u.Path)
			}
			if _, _, _, ok := matchMirroredRepoPath(u.Path); ok {
				return true
			}
			segs := nonEmptySegments(u.Path)
			for i, s := range segs {
				if s == "_git" && i < len(segs)-1 {
					return true
				}
			}
		}
	}
	return !strings.Contains(input, ":") &&
		!strings.HasPrefix(input, ".") &&
		!strings.HasPrefix(input, "/") &&
		regexp.MustCompile(`^([^/]+)/([^/]+)(?:/(.+)|@(.+))?$`).MatchString(input)
}

func parseFragmentRef(input, ghHost string) (without, ref, skillFilter string) {
	idx := strings.IndexByte(input, '#')
	if idx < 0 {
		return input, "", ""
	}
	base := input[:idx]
	frag := input[idx+1:]
	if frag == "" || !looksLikeGitSource(base, ghHost) {
		return input, "", ""
	}
	if at := strings.IndexByte(frag, '@'); at >= 0 {
		r, sf := frag[:at], frag[at+1:]
		return base, decodePart(r), decodePart(sf)
	}
	return base, decodePart(frag), ""
}

func decodePart(s string) string {
	if s == "" {
		return ""
	}
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

func appendFragmentRef(input, ref, skillFilter string) string {
	if ref == "" && skillFilter == "" {
		return input
	}
	var b strings.Builder
	b.WriteString(input)
	b.WriteByte('#')
	if ref != "" {
		b.WriteString(url.PathEscape(ref))
	}
	if skillFilter != "" {
		b.WriteByte('@')
		b.WriteString(url.PathEscape(skillFilter))
	}
	return b.String()
}

func isHostedArtifactURL(input string) bool {
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "raw.githubusercontent.com", "codeload.github.com", "objects.githubusercontent.com":
		return true
	case "github.com":
		return ghHostedArtRe.MatchString(u.Path)
	case "gitlab.com":
		return strings.Contains(u.Path, "/-/archive/") || strings.Contains(u.Path, "/-/raw/")
	default:
		return false
	}
}

func matchMirroredRepoPath(pathname string) (base, ref, subpath string, ok bool) {
	hm := mirrorHostRe.FindStringSubmatch(pathname)
	if hm == nil {
		return "", "", "", false
	}
	prefix, kind, rest := hm[1], strings.ToLower(hm[2]), hm[3]
	var rm []string
	if kind == "github" {
		rm = mirrorGHRestRe.FindStringSubmatch(rest)
	} else {
		rm = mirrorGLRestRe.FindStringSubmatch(rest)
	}
	if rm == nil {
		return "", "", "", false
	}
	return prefix + rm[1], rm[2], rm[3], true
}

func nonEmptySegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseAzureReposSource(input, fragRef, fragSkill string) (*ParsedSource, error) {
	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
		return nil, nil
	}
	u, err := url.Parse(input)
	if err != nil {
		return nil, nil
	}
	segs := nonEmptySegments(u.Path)
	gitIdx := -1
	for i, s := range segs {
		if s == "_git" {
			gitIdx = i
			break
		}
	}
	if gitIdx < 0 || gitIdx == len(segs)-1 {
		return nil, nil
	}
	repo := strings.TrimSuffix(segs[gitIdx+1], ".git")
	if repo == "" {
		return nil, nil
	}
	cloneSegs := append(append([]string{}, segs[:gitIdx]...), "_git", repo)
	for i, s := range cloneSegs {
		cloneSegs[i] = url.PathEscape(decodePart(s))
	}
	cloneURL := fmt.Sprintf("%s://%s/%s", u.Scheme, u.Host, strings.Join(cloneSegs, "/"))
	ref := fragRef
	if ver := u.Query().Get("version"); ver != "" {
		if m := azureVerRe.FindStringSubmatch(ver); m != nil {
			ref = m[1]
		}
	}
	var subpath string
	if p := strings.TrimLeft(u.Query().Get("path"), "/"); p != "" {
		subpath, err = SanitizeSubpath(p)
		if err != nil {
			return nil, err
		}
	}
	return &ParsedSource{
		Type:        SourceGit,
		URL:         cloneURL,
		Ref:         ref,
		Subpath:     subpath,
		SkillFilter: fragSkill,
	}, nil
}

func isWellKnownURL(input string) bool {
	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
		return false
	}
	if strings.HasSuffix(input, ".git") {
		return false
	}
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "gitlab.com", "raw.githubusercontent.com":
		return false
	default:
		return true
	}
}

// ParseSource parses any npx skills source string (local path, GitHub shorthand,
// owner/repo@skill, owner/repo@v1.2.3, GitHub/GitLab/Azure/SSH/mirrored URLs,
// well-known URLs, direct download URLs, or generic git URLs).
func ParseSource(input, cwd, ghHostOverride string) (*ParsedSource, error) {
	orig := input
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("source is empty")
	}

	if isLocalPath(input) {
		resolved, err := resolveLocalPath(input, cwd)
		if err != nil {
			return nil, err
		}
		return &ParsedSource{
			Type:          SourceLocal,
			URL:           resolved,
			LocalPath:     resolved,
			OriginalInput: orig,
		}, nil
	}

	// If input isn't an explicit URL/scheme and matches an existing local directory
	// (or SKILL.md file) relative to cwd — e.g. `hotbed skills add my-skill` or
	// `hotbed skills add skills/my-skill` — upload from that local path directly.
	if !hasExplicitRemotePrefix(input) && !strings.ContainsAny(input, "@#") {
		if candidate, err := resolveLocalPath(input, cwd); err == nil {
			if info, serr := os.Stat(candidate); serr == nil && (info.IsDir() || strings.EqualFold(filepath.Base(candidate), "SKILL.md")) {
				return &ParsedSource{
					Type:          SourceLocal,
					URL:           candidate,
					LocalPath:     candidate,
					OriginalInput: orig,
				}, nil
			}
		}
	}

	ghHost := gitHubHost(ghHostOverride)
	input, fragRef, fragSkill := parseFragmentRef(input, ghHost)

	if alias, ok := sourceAliases[input]; ok {
		input = alias
	}

	if rest, ok := strings.CutPrefix(input, "github:"); ok {
		res, err := ParseSource(appendFragmentRef(rest, fragRef, fragSkill), cwd, ghHostOverride)
		if res != nil {
			res.OriginalInput = orig
		}
		return res, err
	}
	if rest, ok := strings.CutPrefix(input, "gitlab:"); ok {
		res, err := ParseSource(appendFragmentRef("https://gitlab.com/"+rest, fragRef, fragSkill), cwd, ghHostOverride)
		if res != nil {
			res.OriginalInput = orig
		}
		return res, err
	}

	if isHostedArtifactURL(input) {
		return &ParsedSource{Type: SourceDownload, URL: input, OriginalInput: orig}, nil
	}

	// Explicit GitHub Enterprise HTTPS URL when GH_HOST != github.com.
	if ghHost != "github.com" && (strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")) {
		if u, err := url.Parse(input); err == nil && isGitHubHost(u.Host, ghHost) && !strings.EqualFold(u.Host, "github.com") {
			segs := nonEmptySegments(u.Path)
			if len(segs) >= 2 {
				owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
				ref := fragRef
				var subpath string
				if len(segs) >= 4 && segs[2] == "tree" {
					ref = segs[3]
					if len(segs) > 4 {
						sp, err := SanitizeSubpath(strings.Join(segs[4:], "/"))
						if err != nil {
							return nil, err
						}
						subpath = sp
					}
				}
				return &ParsedSource{
					Type:          SourceGit,
					URL:           fmt.Sprintf("%s://%s/%s/%s.git", u.Scheme, u.Host, owner, repo),
					Ref:           ref,
					Subpath:       subpath,
					SkillFilter:   fragSkill,
					OriginalInput: orig,
				}, nil
			}
		}
	}

	// GitHub SSH tree URL.
	if m := sshGitTreeRe.FindStringSubmatch(input); m != nil && isGitHubHost(m[1], ghHost) {
		var sp string
		if m[5] != "" {
			var err error
			if sp, err = SanitizeSubpath(m[5]); err != nil {
				return nil, err
			}
		}
		return &ParsedSource{
			Type:          SourceGit,
			URL:           fmt.Sprintf("git@%s:%s/%s.git", m[1], m[2], m[3]),
			Ref:           m[4],
			Subpath:       sp,
			SkillFilter:   fragSkill,
			OriginalInput: orig,
		}, nil
	}

	// GitHub.com HTTPS URLs.
	if ghPath, ok := pathOnHost(input, "github.com"); ok {
		if m := ghTreeRe.FindStringSubmatch(ghPath); m != nil {
			var sp string
			if m[4] != "" {
				var err error
				if sp, err = SanitizeSubpath(m[4]); err != nil {
					return nil, err
				}
			}
			ref := m[3]
			if ref == "" {
				ref = fragRef
			}
			return &ParsedSource{
				Type:          SourceGitHub,
				URL:           fmt.Sprintf("https://github.com/%s/%s.git", m[1], m[2]),
				Ref:           ref,
				Subpath:       sp,
				SkillFilter:   fragSkill,
				OriginalInput: orig,
			}, nil
		}
		if m := ghRepoRe.FindStringSubmatch(ghPath); m != nil {
			repo := strings.TrimSuffix(m[2], ".git")
			return &ParsedSource{
				Type:          SourceGitHub,
				URL:           fmt.Sprintf("https://github.com/%s/%s.git", m[1], repo),
				Ref:           fragRef,
				SkillFilter:   fragSkill,
				OriginalInput: orig,
			}, nil
		}
	}

	// GitLab tree URL with subpath (any GitLab instance).
	if m := glTreePathRe.FindStringSubmatch(input); m != nil && !strings.EqualFold(m[2], "github.com") {
		sp, err := SanitizeSubpath(m[5])
		if err != nil {
			return nil, err
		}
		ref := m[4]
		if ref == "" {
			ref = fragRef
		}
		return &ParsedSource{
			Type:          SourceGitLab,
			URL:           fmt.Sprintf("%s://%s/%s.git", m[1], m[2], strings.TrimSuffix(m[3], ".git")),
			Ref:           ref,
			Subpath:       sp,
			SkillFilter:   fragSkill,
			OriginalInput: orig,
		}, nil
	}

	// GitLab tree URL without subpath.
	if m := glTreeOnlyRe.FindStringSubmatch(input); m != nil && !strings.EqualFold(m[2], "github.com") {
		ref := m[4]
		if ref == "" {
			ref = fragRef
		}
		return &ParsedSource{
			Type:          SourceGitLab,
			URL:           fmt.Sprintf("%s://%s/%s.git", m[1], m[2], strings.TrimSuffix(m[3], ".git")),
			Ref:           ref,
			SkillFilter:   fragSkill,
			OriginalInput: orig,
		}, nil
	}

	// Official GitLab.com repo URL (including subgroups).
	if glPath, ok := pathOnHost(input, "gitlab.com"); ok {
		if m := glRepoRe.FindStringSubmatch(glPath); m != nil && strings.Contains(m[1], "/") {
			return &ParsedSource{
				Type:          SourceGitLab,
				URL:           fmt.Sprintf("https://gitlab.com/%s.git", m[1]),
				Ref:           fragRef,
				SkillFilter:   fragSkill,
				OriginalInput: orig,
			}, nil
		}
	}

	// Proxy / mirror URLs embedding github.com or gitlab.com.
	if strings.HasPrefix(strings.ToLower(input), "http://") || strings.HasPrefix(strings.ToLower(input), "https://") {
		if u, err := url.Parse(input); err == nil {
			if base, mref, msub, ok := matchMirroredRepoPath(u.Path); ok {
				u.Path = base + ".git"
				u.RawQuery = ""
				u.Fragment = ""
				ref := decodePart(mref)
				if ref == "" {
					ref = fragRef
				}
				var sp string
				if msub != "" {
					sp, err = SanitizeSubpath(decodePart(msub))
					if err != nil {
						return nil, err
					}
				}
				return &ParsedSource{
					Type:          SourceGit,
					URL:           u.String(),
					Ref:           ref,
					Subpath:       sp,
					SkillFilter:   fragSkill,
					OriginalInput: orig,
				}, nil
			}
		}
	}

	// Azure Repos URLs.
	if az, err := parseAzureReposSource(input, fragRef, fragSkill); err != nil {
		return nil, err
	} else if az != nil {
		az.OriginalInput = orig
		return az, nil
	}

	// GitHub shorthand: owner/repo@skill, owner/repo@v1.2.3, owner/repo/subpath.
	shorthandType := SourceGitHub
	if ghHost != "github.com" {
		shorthandType = SourceGit
	}
	if !strings.Contains(input, ":") && !strings.HasPrefix(input, ".") && !strings.HasPrefix(input, "/") {
		if m := atSkillRe.FindStringSubmatch(input); m != nil {
			owner, repo, atPart := m[1], m[2], m[3]
			if fragRef == "" && fragSkill == "" && isVersionPin(atPart) {
				return &ParsedSource{
					Type:          shorthandType,
					URL:           fmt.Sprintf("https://%s/%s/%s.git", ghHost, owner, repo),
					Ref:           atPart,
					OriginalInput: orig,
				}, nil
			}
			sf := fragSkill
			if sf == "" {
				sf = atPart
			}
			return &ParsedSource{
				Type:          shorthandType,
				URL:           fmt.Sprintf("https://%s/%s/%s.git", ghHost, owner, repo),
				Ref:           fragRef,
				SkillFilter:   sf,
				OriginalInput: orig,
			}, nil
		}
		if m := shorthandRe.FindStringSubmatch(input); m != nil {
			owner, repo, rawSub := m[1], m[2], m[3]
			var sp string
			if rawSub != "" {
				var err error
				if sp, err = SanitizeSubpath(rawSub); err != nil {
					return nil, err
				}
			}
			return &ParsedSource{
				Type:          shorthandType,
				URL:           fmt.Sprintf("https://%s/%s/%s.git", ghHost, owner, repo),
				Ref:           fragRef,
				Subpath:       sp,
				SkillFilter:   fragSkill,
				OriginalInput: orig,
			}, nil
		}
	}

	// Well-known HTTP(S) URLs (caller falls back to direct download if no well-known index).
	if isWellKnownURL(input) {
		return &ParsedSource{
			Type:          SourceWellKnown,
			URL:           input,
			OriginalInput: orig,
		}, nil
	}

	// Generic git URL fallback.
	return &ParsedSource{
		Type:          SourceGit,
		URL:           input,
		Ref:           fragRef,
		SkillFilter:   fragSkill,
		OriginalInput: orig,
	}, nil
}

// GetOwnerRepo returns "owner/repo" (or GitLab group/subgroup/repo) for a git source, or "".
func GetOwnerRepo(p *ParsedSource) string {
	if p == nil || p.Type == SourceLocal || p.Type == SourceDownload || p.Type == SourceWellKnown {
		return ""
	}
	if strings.HasPrefix(p.URL, "git@") {
		if _, rest, ok := strings.Cut(p.URL, ":"); ok {
			rest = strings.TrimSuffix(rest, ".git")
			if strings.Contains(rest, "/") {
				return rest
			}
		}
		return ""
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return ""
	}
	pth := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if strings.Contains(pth, "/") {
		return pth
	}
	return ""
}
