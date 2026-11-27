package skillspec

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	wellKnownSchemaV2       = "https://schemas.agentskills.io/discovery/0.2.0/schema.json"
	defaultDownloadMaxBytes = 10 * 1024 * 1024
	defaultExtractMaxBytes  = 25 * 1024 * 1024
	defaultExtractMaxFiles  = 1000
	defaultSkillsAPIBase    = "https://skills.sh"
)

// FetchOptions controls how a source is fetched and discovered.
type FetchOptions struct {
	Cwd             string
	GitHubHost      string
	Ref             string // overrides ref parsed from source when non-empty
	IncludeInternal bool
	FullDepth       bool
}

// FetchResult holds the skills discovered from a source plus upstream provenance.
type FetchResult struct {
	Parsed     *ParsedSource
	Skills     []DiscoveredSkill
	SourceType string
	Source     string // canonical source identifier (owner/repo, URL, or local path)
	SourceURL  string
	Ref        string
	Commit     string
	Subpath    string
}

func positiveIntEnv(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return n
	}
	return fallback
}

// FetchSource resolves, fetches (via local fs, git, well-known, or direct download),
// and discovers all skills from an npx skills-compatible source string.
func FetchSource(ctx context.Context, client *http.Client, input string, opts FetchOptions) (*FetchResult, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	parsed, err := ParseSource(input, opts.Cwd, opts.GitHubHost)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Ref) != "" {
		parsed.Ref = strings.TrimSpace(opts.Ref)
	}
	discOpts := DiscoverOptions{
		IncludeInternal: opts.IncludeInternal || parsed.SkillFilter != "",
		FullDepth:       opts.FullDepth,
	}

	switch parsed.Type {
	case SourceLocal:
		info, err := os.Stat(parsed.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("local path %q: %w", parsed.LocalPath, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("local path %q is not a directory", parsed.LocalPath)
		}
		skills, err := DiscoverSkills(parsed.LocalPath, "", discOpts)
		if err != nil {
			return nil, err
		}
		return &FetchResult{
			Parsed:     parsed,
			Skills:     skills,
			SourceType: string(SourceLocal),
			Source:     parsed.LocalPath,
			SourceURL:  parsed.LocalPath,
		}, nil

	case SourceGitHub, SourceGitLab, SourceGit:
		return fetchGitSource(ctx, parsed, discOpts)

	case SourceWellKnown:
		res, ok, err := fetchWellKnownSource(ctx, client, parsed, discOpts)
		if err != nil {
			return nil, err
		}
		if ok {
			return res, nil
		}
		// Exact npx skills behavior: fall back to direct SKILL.md / archive download.
		return fetchDownloadSource(ctx, client, parsed, discOpts)

	case SourceDownload:
		return fetchDownloadSource(ctx, client, parsed, discOpts)

	default:
		return nil, fmt.Errorf("unsupported skill source type %q", parsed.Type)
	}
}

func fetchGitSource(ctx context.Context, parsed *ParsedSource, discOpts DiscoverOptions) (*FetchResult, error) {
	tmpDir, err := os.MkdirTemp("", "hotbed-skills-git-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	if err := cloneGitRepo(ctx, parsed.URL, parsed.Ref, tmpDir); err != nil {
		return nil, err
	}
	commit := gitHeadCommit(ctx, tmpDir)
	skills, err := DiscoverSkills(tmpDir, parsed.Subpath, discOpts)
	if err != nil {
		return nil, err
	}
	srcID := GetOwnerRepo(parsed)
	if srcID == "" {
		srcID = parsed.URL
	}
	return &FetchResult{
		Parsed:     parsed,
		Skills:     skills,
		SourceType: string(parsed.Type),
		Source:     srcID,
		SourceURL:  parsed.URL,
		Ref:        parsed.Ref,
		Commit:     commit,
		Subpath:    parsed.Subpath,
	}, nil
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_ALLOW_PROTOCOL=https:http:ssh:git:file",
		"LC_ALL=C",
	)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func isMissingRefError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found in upstream origin") ||
		strings.Contains(lower, "couldn't find remote ref") ||
		strings.Contains(lower, "upload-pack: not our ref")
}

func isAuthFailure(msg string) bool {
	return strings.Contains(msg, "Authentication failed") ||
		strings.Contains(msg, "could not read Username") ||
		strings.Contains(msg, "Permission denied") ||
		strings.Contains(msg, "Repository not found") ||
		strings.Contains(msg, "returned error: 403")
}

func cloneGitRepo(ctx context.Context, repoURL, ref, dst string) error {
	cloneArgs := []string{"clone", "--depth", "1"}
	if ref != "" {
		cloneArgs = append(cloneArgs, "--branch", ref)
	}
	cloneArgs = append(cloneArgs, repoURL, dst)
	out, err := runGit(ctx, "", cloneArgs...)
	if err == nil {
		return nil
	}

	// If ref is a 40-char commit SHA, git clone --branch fails; fetch by SHA.
	if ref != "" && IsCommitSHA(ref) && isMissingRefError(out) {
		_ = os.RemoveAll(dst)
		if mkErr := os.MkdirAll(dst, 0o755); mkErr != nil {
			return mkErr
		}
		if err2 := cloneAtSHA(ctx, repoURL, ref, dst); err2 == nil {
			return nil
		}
	}

	// Private GitHub HTTPS fallback: try `gh repo clone` then SSH (matching npx skills).
	if isAuthFailure(out) {
		if ownerRepo, sshURL, ok := parseGitHubHTTPS(repoURL); ok {
			_ = os.RemoveAll(dst)
			if ghErr := cloneWithGH(ctx, ownerRepo, ref, dst); ghErr == nil {
				return nil
			}
			_ = os.RemoveAll(dst)
			sshArgs := []string{"clone", "--depth", "1"}
			if ref != "" {
				sshArgs = append(sshArgs, "--branch", ref)
			}
			sshArgs = append(sshArgs, sshURL, dst)
			if _, sshErr := runGit(ctx, "", sshArgs...); sshErr == nil {
				return nil
			}
		}
	}

	return err
}

func cloneAtSHA(ctx context.Context, repoURL, sha, dst string) error {
	if _, err := runGit(ctx, dst, "init"); err != nil {
		return err
	}
	if _, err := runGit(ctx, dst, "remote", "add", "origin", repoURL); err != nil {
		return err
	}
	if _, err := runGit(ctx, dst, "fetch", "--depth", "1", "origin", sha); err != nil {
		return err
	}
	_, err := runGit(ctx, dst, "checkout", "FETCH_HEAD")
	return err
}

func parseGitHubHTTPS(rawURL string) (ownerRepo, sshURL string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") {
		return "", "", false
	}
	parts := nonEmptySegments(strings.TrimSuffix(u.Path, ".git"))
	if len(parts) != 2 {
		return "", "", false
	}
	or := parts[0] + "/" + parts[1]
	return or, "git@github.com:" + or + ".git", true
}

func cloneWithGH(ctx context.Context, ownerRepo, ref, dst string) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return err
	}
	args := []string{"repo", "clone", ownerRepo, dst, "--", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh repo clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitHeadCommit(ctx context.Context, dir string) string {
	sha, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return sha
}

// ---- RFC 8615 Well-Known Skills Discovery (v0.2.0 and legacy v0.1.0) ----

var wellKnownPaths = []string{".well-known/agent-skills", ".well-known/skills"}

type wellKnownIndexRaw struct {
	Schema string            `json:"$schema"`
	Skills []json.RawMessage `json:"skills"`
}

type wellKnownEntryV1 struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

type wellKnownEntryV2 struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Digest      string `json:"digest"`
}

func fetchWellKnownSource(ctx context.Context, client *http.Client, parsed *ParsedSource, discOpts DiscoverOptions) (*FetchResult, bool, error) {
	u, err := url.Parse(parsed.URL)
	if err != nil {
		return nil, false, nil
	}
	basePath := strings.TrimRight(u.Path, "/")
	type probe struct {
		indexURL string
		baseURL  string
		wkPath   string
	}
	var probes []probe
	for _, wk := range wellKnownPaths {
		probes = append(probes, probe{
			indexURL: fmt.Sprintf("%s://%s%s/%s/index.json", u.Scheme, u.Host, basePath, wk),
			baseURL:  fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, basePath),
			wkPath:   wk,
		})
		if basePath != "" {
			probes = append(probes, probe{
				indexURL: fmt.Sprintf("%s://%s/%s/index.json", u.Scheme, u.Host, wk),
				baseURL:  fmt.Sprintf("%s://%s", u.Scheme, u.Host),
				wkPath:   wk,
			})
		}
	}

	for _, pr := range probes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.indexURL, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		var raw wellKnownIndexRaw
		if err := json.Unmarshal(body, &raw); err != nil || raw.Skills == nil {
			continue
		}
		var discovered []DiscoveredSkill
		if raw.Schema == wellKnownSchemaV2 {
			for _, elem := range raw.Skills {
				var e wellKnownEntryV2
				if err := json.Unmarshal(elem, &e); err != nil {
					continue
				}
				if ValidateName(e.Name) != nil || e.Description == "" || (e.Type != "skill-md" && e.Type != "archive") || e.URL == "" {
					continue
				}
				artURL, err := resolveRelativeURL(pr.indexURL, e.URL)
				if err != nil {
					continue
				}
				sk, err := fetchWellKnownV2Skill(ctx, client, e, artURL, discOpts)
				if err != nil {
					return nil, true, err
				}
				if sk != nil {
					discovered = append(discovered, *sk)
				}
			}
			if len(discovered) == 0 {
				continue
			}
		} else if raw.Schema == "" {
			// Legacy v0.1.0 format
			for _, elem := range raw.Skills {
				var e wellKnownEntryV1
				if err := json.Unmarshal(elem, &e); err != nil {
					continue
				}
				if ValidateName(e.Name) != nil || e.Description == "" || len(e.Files) == 0 {
					continue
				}
				sk, err := fetchWellKnownV1Skill(ctx, client, pr.baseURL, pr.wkPath, e, discOpts)
				if err != nil {
					return nil, true, err
				}
				if sk != nil {
					discovered = append(discovered, *sk)
				}
			}
			if len(discovered) == 0 {
				continue
			}
		} else {
			continue
		}

		hostID := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		return &FetchResult{
			Parsed:     parsed,
			Skills:     discovered,
			SourceType: string(SourceWellKnown),
			Source:     hostID,
			SourceURL:  parsed.URL,
		}, true, nil
	}
	return nil, false, nil
}

func resolveRelativeURL(baseStr, refStr string) (string, error) {
	base, err := url.Parse(baseStr)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(refStr)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func fetchWellKnownV2Skill(ctx context.Context, client *http.Client, e wellKnownEntryV2, artURL string, discOpts DiscoverOptions) (*DiscoveredSkill, error) {
	maxBytes := positiveIntEnv("SKILLS_DOWNLOAD_MAX_BYTES", defaultDownloadMaxBytes)
	data, err := httpDownloadBytes(ctx, client, artURL, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("well-known skill %q: %w", e.Name, err)
	}
	if e.Digest != "" {
		got := SHA256Digest(data)
		if got != e.Digest {
			return nil, fmt.Errorf("well-known skill %q digest mismatch: got %s, want %s", e.Name, got, e.Digest)
		}
	}
	var files []FileEntry
	if e.Type == "skill-md" {
		files = []FileEntry{{
			Path:     "SKILL.md",
			Content:  data,
			MIMEType: "text/markdown",
			Digest:   SHA256Digest(data),
		}}
	} else {
		files, err = extractArchiveToEntries(data,
			positiveIntEnv("SKILLS_EXTRACT_MAX_BYTES", defaultExtractMaxBytes),
			int(positiveIntEnv("SKILLS_EXTRACT_MAX_FILES", defaultExtractMaxFiles)))
		if err != nil {
			return nil, fmt.Errorf("well-known skill %q archive: %w", e.Name, err)
		}
	}
	return buildDiscoveredFromFiles(files, discOpts)
}

func fetchWellKnownV1Skill(ctx context.Context, client *http.Client, baseURL, wkPath string, e wellKnownEntryV1, discOpts DiscoverOptions) (*DiscoveredSkill, error) {
	maxBytes := positiveIntEnv("SKILLS_DOWNLOAD_MAX_BYTES", defaultDownloadMaxBytes)
	var files []FileEntry
	for _, fpath := range e.Files {
		cleanPath, err := sanitizeArchivePath(fpath)
		if err != nil || cleanPath == "" {
			return nil, fmt.Errorf("well-known skill %q unsafe file path %q", e.Name, fpath)
		}
		fileURL := fmt.Sprintf("%s/%s/%s/%s", baseURL, wkPath, e.Name, cleanPath)
		data, err := httpDownloadBytes(ctx, client, fileURL, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("well-known skill %q file %q: %w", e.Name, cleanPath, err)
		}
		files = append(files, FileEntry{
			Path:     cleanPath,
			Content:  data,
			MIMEType: InferMIMEType(cleanPath, data),
			Digest:   SHA256Digest(data),
			Binary:   IsBinary(data),
		})
	}
	return buildDiscoveredFromFiles(files, discOpts)
}

func buildDiscoveredFromFiles(files []FileEntry, discOpts DiscoverOptions) (*DiscoveredSkill, error) {
	slices.SortFunc(files, func(a, b FileEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	var skillMD []byte
	for _, f := range files {
		if f.Path == "SKILL.md" {
			skillMD = f.Content
			break
		}
	}
	if skillMD == nil {
		return nil, fmt.Errorf("skill package is missing SKILL.md")
	}
	fm, _, err := ParseFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}
	name, _ := fm["name"].(string)
	desc, _ := fm["description"].(string)
	name = strings.TrimSpace(name)
	desc = strings.TrimSpace(desc)
	if name == "" || desc == "" {
		return nil, fmt.Errorf("SKILL.md frontmatter missing required name or description")
	}
	if IsInternalSkill(fm) && !shouldInstallInternal() && !discOpts.IncludeInternal {
		return nil, nil
	}
	fmJSON, err := FrontmatterJSON(fm)
	if err != nil {
		return nil, err
	}
	return &DiscoveredSkill{
		Name:            name,
		Description:     desc,
		Files:           files,
		Frontmatter:     fm,
		FrontmatterJSON: fmJSON,
		ContentHash:     HashSkillFiles(files),
	}, nil
}

// ---- Direct HTTP Download (SKILL.md or .zip / .tar / .tar.gz / .tgz) ----

func fetchDownloadSource(ctx context.Context, client *http.Client, parsed *ParsedSource, discOpts DiscoverOptions) (*FetchResult, error) {
	maxBytes := positiveIntEnv("SKILLS_DOWNLOAD_MAX_BYTES", defaultDownloadMaxBytes)
	data, err := httpDownloadBytes(ctx, client, parsed.URL, maxBytes)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("downloaded file from %s is empty", parsed.URL)
	}

	// 1. Check if it is a single SKILL.md directly.
	if fm, _, fmErr := ParseFrontmatter(data); fmErr == nil {
		if n, _ := fm["name"].(string); strings.TrimSpace(n) != "" {
			if d, _ := fm["description"].(string); strings.TrimSpace(d) != "" {
				sk, err := buildDiscoveredFromFiles([]FileEntry{{
					Path:     "SKILL.md",
					Content:  data,
					MIMEType: "text/markdown",
					Digest:   SHA256Digest(data),
				}}, discOpts)
				if err != nil {
					return nil, err
				}
				var skills []DiscoveredSkill
				if sk != nil {
					skills = append(skills, *sk)
				}
				return &FetchResult{
					Parsed:     parsed,
					Skills:     skills,
					SourceType: string(SourceDownload),
					Source:     cleanDownloadSourceURL(parsed.URL),
					SourceURL:  cleanDownloadSourceURL(parsed.URL),
				}, nil
			}
		}
	}

	// 2. Otherwise extract as .zip / .tar.gz / .tar into a temp dir and discover skills.
	tmpDir, err := os.MkdirTemp("", "hotbed-skills-dl-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	extractMaxBytes := positiveIntEnv("SKILLS_EXTRACT_MAX_BYTES", defaultExtractMaxBytes)
	extractMaxFiles := int(positiveIntEnv("SKILLS_EXTRACT_MAX_FILES", defaultExtractMaxFiles))
	entries, err := extractArchiveToEntries(data, extractMaxBytes, extractMaxFiles)
	if err != nil {
		return nil, fmt.Errorf("download %s is neither a valid SKILL.md nor a supported archive (.zip, .tar.gz, .tar): %w", parsed.URL, err)
	}
	for _, f := range entries {
		dst := filepath.Join(tmpDir, filepath.FromSlash(f.Path))
		if !isSubpathSafe(tmpDir, f.Path) {
			return nil, fmt.Errorf("unsafe archive path %q", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, f.Content, 0o644); err != nil {
			return nil, err
		}
	}
	rootDir := singleTopLevelDir(tmpDir)
	skills, err := DiscoverSkills(rootDir, "", discOpts)
	if err != nil {
		return nil, err
	}
	return &FetchResult{
		Parsed:     parsed,
		Skills:     skills,
		SourceType: string(SourceDownload),
		Source:     cleanDownloadSourceURL(parsed.URL),
		SourceURL:  cleanDownloadSourceURL(parsed.URL),
	}, nil
}

func cleanDownloadSourceURL(raw string) string {
	// Per npx skills docs: omit URLs with query strings or userinfo (signed URLs/credentials) from provenance.
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.RawQuery != "" || u.User != nil {
		return ""
	}
	return raw
}

func singleTopLevelDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}
	var visible []os.DirEntry
	for _, e := range entries {
		if e.Name() != "__MACOSX" {
			visible = append(visible, e)
		}
	}
	if len(visible) == 1 && visible[0].IsDir() {
		return filepath.Join(dir, visible[0].Name())
	}
	return dir
}

func httpDownloadBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("download exceeds %d byte limit", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("download exceeds %d byte limit", maxBytes)
	}
	return data, nil
}

func sanitizeArchivePath(raw string) (string, error) {
	p := strings.ReplaceAll(raw, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || strings.HasSuffix(p, "/") {
		return "", nil
	}
	if strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || winAbsRe.MatchString(p) {
		return "", fmt.Errorf("unsafe archive path %q", raw)
	}
	parts := strings.Split(p, "/")
	var clean []string
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("unsafe archive path %q", raw)
		}
		clean = append(clean, part)
	}
	return strings.Join(clean, "/"), nil
}

func extractArchiveToEntries(data []byte, maxBytes int64, maxFiles int) ([]FileEntry, error) {
	if len(data) >= 2 && data[0] == 0x50 && data[1] == 0x4b {
		return extractZipEntries(data, maxBytes, maxFiles)
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return extractTarEntries(gz, maxBytes, maxFiles)
	}
	return extractTarEntries(bytes.NewReader(data), maxBytes, maxFiles)
}

func extractZipEntries(data []byte, maxBytes int64, maxFiles int) ([]FileEntry, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var out []FileEntry
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("archive contains symlink %q", f.Name)
		}
		p, err := sanitizeArchivePath(f.Name)
		if err != nil {
			return nil, err
		}
		if p == "" {
			continue
		}
		if len(out)+1 > maxFiles {
			return nil, fmt.Errorf("archive exceeds %d files", maxFiles)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxBytes-total+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(content))
		if total > maxBytes {
			return nil, fmt.Errorf("archive unpacked content exceeds %d bytes", maxBytes)
		}
		out = append(out, FileEntry{
			Path:     p,
			Content:  content,
			MIMEType: InferMIMEType(p, content),
			Digest:   SHA256Digest(content),
			Binary:   IsBinary(content),
		})
	}
	return out, nil
}

func extractTarEntries(r io.Reader, maxBytes int64, maxFiles int) ([]FileEntry, error) {
	tr := tar.NewReader(r)
	var out []FileEntry
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return nil, fmt.Errorf("archive contains link entry %q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		p, err := sanitizeArchivePath(hdr.Name)
		if err != nil {
			return nil, err
		}
		if p == "" {
			continue
		}
		if len(out)+1 > maxFiles {
			return nil, fmt.Errorf("archive exceeds %d files", maxFiles)
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxBytes-total+1))
		if err != nil {
			return nil, err
		}
		total += int64(len(content))
		if total > maxBytes {
			return nil, fmt.Errorf("archive unpacked content exceeds %d bytes", maxBytes)
		}
		out = append(out, FileEntry{
			Path:     p,
			Content:  content,
			MIMEType: InferMIMEType(p, content),
			Digest:   SHA256Digest(content),
			Binary:   IsBinary(content),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("archive contains no files")
	}
	return out, nil
}

// ---- skills.sh Search API (matching `npx skills find`) ----

// SearchSkill is one skill returned by the skills.sh search API.
type SearchSkill struct {
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Source   string `json:"source"`
	Installs int    `json:"installs"`
}

// SearchSkillsAPI queries https://skills.sh/api/search (or apiBase / SKILLS_API_URL).
func SearchSkillsAPI(ctx context.Context, client *http.Client, query, owner, apiBase string) ([]SearchSkill, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(os.Getenv("SKILLS_API_URL")), "/")
	}
	if base == "" {
		base = defaultSkillsAPIBase
	}
	params := url.Values{}
	params.Set("q", query)
	if strings.TrimSpace(owner) != "" {
		params.Set("owner", strings.ToLower(strings.TrimSpace(owner)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/search?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh search returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Skills []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Source   string `json:"source"`
			Installs int    `json:"installs"`
		} `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]SearchSkill, 0, len(payload.Skills))
	for _, s := range payload.Skills {
		out = append(out, SearchSkill{
			Name:     s.Name,
			Slug:     s.ID,
			Source:   s.Source,
			Installs: s.Installs,
		})
	}
	return out, nil
}
