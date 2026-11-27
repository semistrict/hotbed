package skillspec

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseSourceFormats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantTyp SourceType
		wantURL string
		wantRef string
		wantSub string
		wantSF  string
	}{
		{"vercel-labs/agent-skills", SourceGitHub, "https://github.com/vercel-labs/agent-skills.git", "", "", ""},
		{"vercel-labs/vercel-skills", SourceGitHub, "https://github.com/vercel-labs/agent-skills.git", "", "", ""},
		{"vercel-labs/agent-skills@web-design-guidelines", SourceGitHub, "https://github.com/vercel-labs/agent-skills.git", "", "", "web-design-guidelines"},
		{"owner/repo@v1.2.3", SourceGitHub, "https://github.com/owner/repo.git", "v1.2.3", "", ""},
		{"owner/repo@0123456789abcdef0123456789abcdef01234567", SourceGitHub, "https://github.com/owner/repo.git", "0123456789abcdef0123456789abcdef01234567", "", ""},
		{"owner/repo#main@my-skill", SourceGitHub, "https://github.com/owner/repo.git", "main", "", "my-skill"},
		{"owner/repo/skills/foo", SourceGitHub, "https://github.com/owner/repo.git", "", "skills/foo", ""},
		{"github:owner/repo@my-skill", SourceGitHub, "https://github.com/owner/repo.git", "", "", "my-skill"},
		{"gitlab:group/sub/repo", SourceGitLab, "https://gitlab.com/group/sub/repo.git", "", "", ""},
		{"https://github.com/vercel-labs/agent-skills/tree/main/skills/web-design", SourceGitHub, "https://github.com/vercel-labs/agent-skills.git", "main", "skills/web-design", ""},
		{"https://gitlab.com/org/sub/repo/-/tree/dev/skills/demo", SourceGitLab, "https://gitlab.com/org/sub/repo.git", "dev", "skills/demo", ""},
		{"https://dev.azure.com/org/proj/_git/repo?path=/skills/demo&version=GBmain", SourceGit, "https://dev.azure.com/org/proj/_git/repo", "main", "skills/demo", ""},
		{"git@github.com:acme/private.git", SourceGit, "git@github.com:acme/private.git", "", "", ""},
		{"https://raw.githubusercontent.com/o/r/main/SKILL.md", SourceDownload, "https://raw.githubusercontent.com/o/r/main/SKILL.md", "", "", ""},
		{"https://docs.example.com", SourceWellKnown, "https://docs.example.com", "", "", ""},
		{"./local-skills", SourceLocal, "/work/local-skills", "", "", ""},
	}
	for _, tc := range cases {
		got, err := ParseSource(tc.in, "/work", "github.com")
		if err != nil {
			t.Errorf("ParseSource(%q) err: %v", tc.in, err)
			continue
		}
		if got.Type != tc.wantTyp || got.URL != tc.wantURL || got.Ref != tc.wantRef || got.Subpath != tc.wantSub || got.SkillFilter != tc.wantSF {
			t.Errorf("ParseSource(%q) = %+v, want type=%s url=%s ref=%s sub=%s sf=%s",
				tc.in, got, tc.wantTyp, tc.wantURL, tc.wantRef, tc.wantSub, tc.wantSF)
		}
	}
}

func TestParseAndFetchLocalDirVariants(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	skillDir := filepath.Join(cwd, "skills", "my-local-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: my-local-skill\ndescription: Uploaded from a local directory.\n---\n\n# Local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("Guide content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bare relative dir ("skills"), two-segment relative dir ("skills/my-local-skill"),
	// and direct SKILL.md path all resolve and upload as local skills rather than GitHub shorthand.
	for _, input := range []string{"skills", "skills/my-local-skill", "./skills/my-local-skill/SKILL.md"} {
		res, err := FetchSource(t.Context(), nil, input, FetchOptions{Cwd: cwd})
		if err != nil {
			t.Fatalf("FetchSource(%q) err: %v", input, err)
		}
		if res.SourceType != string(SourceLocal) {
			t.Errorf("FetchSource(%q) sourceType = %q, want %q", input, res.SourceType, SourceLocal)
		}
		if len(res.Skills) != 1 || res.Skills[0].Name != "my-local-skill" || len(res.Skills[0].Files) != 2 {
			t.Errorf("FetchSource(%q) skills = %+v, want 1 skill with 2 files", input, res.Skills)
		}
	}

	// Tilde path expands against home directory.
	gotHome, err := ParseSource("~/my-skills", cwd, "github.com")
	if err != nil {
		t.Fatalf("ParseSource(~/my-skills) err: %v", err)
	}
	home, _ := os.UserHomeDir()
	if gotHome.Type != SourceLocal || gotHome.LocalPath != filepath.Join(home, "my-skills") {
		t.Errorf("ParseSource(~/my-skills) = %+v", gotHome)
	}
}

func TestFrontmatterParseRenameAndHash(t *testing.T) {
	t.Parallel()
	md := []byte("---\nname: pdf-tools\ndescription: Extract and merge PDFs.\nlicense: MIT\nmetadata:\n  author: acme\n  date: 2026-11-27\n---\n\n# PDF Tools\nUse carefully.\n")
	fm, body, err := ParseFrontmatter(md)
	if err != nil {
		t.Fatal(err)
	}
	if fm["name"] != "pdf-tools" || fm["description"] != "Extract and merge PDFs." || fm["license"] != "MIT" {
		t.Fatalf("unexpected frontmatter: %#v", fm)
	}
	meta := fm["metadata"].(map[string]any)
	if meta["date"] != "2026-11-27" {
		t.Errorf("expected timestamp preserved as string, got %#v (%T)", meta["date"], meta["date"])
	}
	if !strings.Contains(body, "# PDF Tools") {
		t.Errorf("unexpected body: %q", body)
	}

	renamed, err := RewriteFrontmatterName(md, "acme-pdf")
	if err != nil {
		t.Fatal(err)
	}
	fm2, _, err := ParseFrontmatter(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if fm2["name"] != "acme-pdf" || fm2["license"] != "MIT" {
		t.Errorf("renamed frontmatter = %#v", fm2)
	}

	// Verify CRLF normalization and metadata.json exclusion in HashSkillFiles.
	h1 := HashSkillFiles([]FileEntry{
		{Path: "SKILL.md", Content: []byte("line1\nline2\n")},
		{Path: "metadata.json", Content: []byte(`{"ignored":true}`)},
	})
	h2 := HashSkillFiles([]FileEntry{
		{Path: "SKILL.md", Content: []byte("line1\r\nline2\r\n")},
	})
	if h1 != h2 {
		t.Errorf("hash mismatch across CRLF/metadata.json: %s vs %s", h1, h2)
	}
}

func TestDiscoverSkillsPriorityAndInternal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("skills/alpha/SKILL.md", "---\nname: alpha\ndescription: Alpha skill\n---\n# Alpha\n")
	writeFile("skills/alpha/references/guide.md", "guide\n")
	writeFile("skills/.curated/beta/SKILL.md", "---\nname: beta\ndescription: Beta skill\n---\n# Beta\n")
	writeFile("skills/secret/SKILL.md", "---\nname: secret\ndescription: Internal skill\nmetadata:\n  internal: \"true\"\n---\n# Secret\n")

	found, err := DiscoverSkills(root, "", DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range found {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Fatalf("discovered %v, want [alpha beta]", names)
	}

	// IncludeInternal reveals the internal skill.
	foundAll, err := DiscoverSkills(root, "", DiscoverOptions{IncludeInternal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(foundAll) != 3 {
		t.Fatalf("with IncludeInternal got %d skills, want 3", len(foundAll))
	}
}

func TestFetchWellKnownV2AndDirectArchives(t *testing.T) {
	t.Parallel()
	skillMD := []byte("---\nname: web-audit\ndescription: Audit web pages.\n---\n# Web Audit\n")
	refMD := []byte("# Ref\n")

	// Build a tar.gz archive for v0.2.0 well-known archive test.
	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	for name, content := range map[string][]byte{"SKILL.md": skillMD, "references/ref.md": refMD} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gzw.Close()
	tarBytes := tarBuf.Bytes()

	// Build a zip archive for direct download fallback test.
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("bundle/SKILL.md")
	_, _ = w.Write([]byte("---\nname: zip-skill\ndescription: From zip archive.\n---\n# Zip\n"))
	zw.Close()
	zipBytes := zipBuf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/agent-skills/index.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"$schema": wellKnownSchemaV2,
				"skills": []map[string]any{{
					"name":        "web-audit",
					"type":        "archive",
					"description": "Audit web pages.",
					"url":         "/artifacts/web-audit.tar.gz",
					"digest":      SHA256Digest(tarBytes),
				}},
			})
		case "/artifacts/web-audit.tar.gz":
			_, _ = w.Write(tarBytes)
		case "/dl/bundle.zip":
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res, err := FetchSource(t.Context(), srv.Client(), srv.URL, FetchOptions{})
	if err != nil {
		t.Fatalf("FetchSource well-known: %v", err)
	}
	if res.SourceType != string(SourceWellKnown) || len(res.Skills) != 1 || res.Skills[0].Name != "web-audit" || len(res.Skills[0].Files) != 2 {
		t.Fatalf("unexpected well-known result: %+v", res)
	}

	// Direct download fallback (no well-known index under /dl -> actually root has well-known, so test direct URL on separate server).
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl/bundle.zip" {
			_, _ = w.Write(zipBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer dlSrv.Close()

	dlRes, err := FetchSource(t.Context(), dlSrv.Client(), dlSrv.URL+"/dl/bundle.zip", FetchOptions{})
	if err != nil {
		t.Fatalf("FetchSource direct zip: %v", err)
	}
	if dlRes.SourceType != string(SourceDownload) || len(dlRes.Skills) != 1 || dlRes.Skills[0].Name != "zip-skill" {
		t.Fatalf("unexpected download result: %+v", dlRes)
	}
}

func TestInstallToAgents(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cwd := t.TempDir()
	agents, err := ResolveAgents(home, []string{"claude-code", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	files := []FileEntry{
		{Path: "SKILL.md", Content: []byte("---\nname: demo\ndescription: Demo\n---\n# Demo\n")},
		{Path: "references/a.md", Content: []byte("ref\n")},
	}
	res, err := InstallToAgents("demo", files, LocalInstallOptions{
		Global: true,
		Home:   home,
		Cwd:    cwd,
		Agents: agents,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "demo", "SKILL.md")); err != nil {
		t.Fatalf("missing canonical SKILL.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "demo", "references", "a.md")); err != nil {
		t.Fatalf("missing claude symlinked ref: %v", err)
	}
	if len(res.AgentDirs) != 2 {
		t.Errorf("AgentDirs = %v, want 2", res.AgentDirs)
	}
}
