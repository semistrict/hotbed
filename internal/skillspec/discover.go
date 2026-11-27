package skillspec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DiscoveredSkill is a valid Agent Skill discovered from a repository, archive,
// well-known endpoint, or local directory.
type DiscoveredSkill struct {
	Name            string
	Description     string
	Dir             string
	Subpath         string // relative path inside the source root, if any
	Files           []FileEntry
	Frontmatter     map[string]any
	FrontmatterJSON string
	ContentHash     string
}

// DiscoverOptions controls skill discovery behavior.
type DiscoverOptions struct {
	IncludeInternal bool
	FullDepth       bool
}

var skipWalkDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"build":        true,
	"__pycache__":  true,
}

// Standard agent project skill directories searched by npx skills.
var agentProjectSkillDirs = []string{
	".agents/skills",
	".claude/skills",
	".codex/skills",
	".cursor/skills",
	".opencode/skills",
	".gemini/skills",
	".github/skills",
	".windsurf/skills",
	".cline/skills",
	".roo/skills",
	".kilocode/skills",
	".kiro/skills",
	".goose/skills",
	".amp/skills",
	".trae/skills",
	".augment/skills",
	".continue/skills",
	".vibe/skills",
	".factory/skills",
	".openhands/skills",
}

const defaultContainerDepth = 3

func shouldInstallInternal() bool {
	v := os.Getenv("INSTALL_INTERNAL_SKILLS")
	return v == "1" || strings.EqualFold(v, "true")
}

func hasSkillMD(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !info.IsDir()
}

func isSubpathSafe(basePath, subpath string) bool {
	absBase, err := filepath.Abs(basePath)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(filepath.Join(absBase, subpath))
	if err != nil {
		return false
	}
	return absTarget == absBase || strings.HasPrefix(absTarget, absBase+string(filepath.Separator))
}

func parseSkillDir(basePath, skillDir string, opts DiscoverOptions) (*DiscoveredSkill, error) {
	files, err := CollectSkillFiles(skillDir)
	if err != nil {
		return nil, err
	}
	var skillMD []byte
	for _, f := range files {
		if f.Path == "SKILL.md" {
			skillMD = f.Content
			break
		}
	}
	if skillMD == nil {
		return nil, nil
	}
	fm, _, err := ParseFrontmatter(skillMD)
	if err != nil {
		return nil, nil
	}
	rawName, _ := fm["name"].(string)
	rawDesc, _ := fm["description"].(string)
	name := strings.TrimSpace(rawName)
	desc := strings.TrimSpace(rawDesc)
	if name == "" || desc == "" {
		return nil, nil
	}
	if IsInternalSkill(fm) && !shouldInstallInternal() && !opts.IncludeInternal {
		return nil, nil
	}
	fmJSON, err := FrontmatterJSON(fm)
	if err != nil {
		return nil, err
	}
	relSub := ""
	if rel, err := filepath.Rel(basePath, skillDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		relSub = filepath.ToSlash(rel)
	}
	return &DiscoveredSkill{
		Name:            name,
		Description:     desc,
		Dir:             skillDir,
		Subpath:         relSub,
		Files:           files,
		Frontmatter:     fm,
		FrontmatterJSON: fmJSON,
		ContentHash:     HashSkillFiles(files),
	}, nil
}

// DiscoverSkills discovers valid Agent Skills under basePath (optionally narrowed
// by subpath) using the exact priority-order and fallback rules of npx skills.
func DiscoverSkills(basePath, subpath string, opts DiscoverOptions) ([]DiscoveredSkill, error) {
	if subpath != "" && !isSubpathSafe(basePath, subpath) {
		return nil, fmt.Errorf("subpath %q escapes source root", subpath)
	}
	searchPath := basePath
	if subpath != "" {
		searchPath = filepath.Join(basePath, filepath.FromSlash(subpath))
	}

	var out []DiscoveredSkill
	seenNames := map[string]bool{}
	seenPaths := map[string]bool{}

	tryAdd := func(dir string) bool {
		if !hasSkillMD(dir) {
			return false
		}
		mdPath := filepath.Join(dir, "SKILL.md")
		canon := mdPath
		if r, err := filepath.EvalSymlinks(mdPath); err == nil {
			canon = r
		}
		if seenPaths[canon] {
			return true
		}
		seenPaths[canon] = true
		sk, err := parseSkillDir(basePath, dir, opts)
		if err != nil || sk == nil {
			return true
		}
		if seenNames[sk.Name] {
			return true
		}
		seenNames[sk.Name] = true
		out = append(out, *sk)
		return true
	}

	// Direct root SKILL.md returns early unless --full-depth is set.
	if tryAdd(searchPath) && len(out) > 0 && !opts.FullDepth {
		return out, nil
	}

	priorityDirs := []string{
		searchPath,
		filepath.Join(searchPath, "skills"),
		filepath.Join(searchPath, "skills", ".curated"),
		filepath.Join(searchPath, "skills", ".experimental"),
		filepath.Join(searchPath, "skills", ".system"),
	}
	for _, d := range agentProjectSkillDirs {
		priorityDirs = append(priorityDirs, filepath.Join(searchPath, filepath.FromSlash(d)))
	}
	priorityDirs = append(priorityDirs, readSkillsShPaths(searchPath)...)

	deepDirs := map[string]bool{}
	for _, d := range priorityDirs[1:] {
		deepDirs[d] = true
	}
	priorityDirs = append(priorityDirs, readPluginManifestSkillDirs(searchPath)...)

	var walkContainer func(dir string, maxDepth, depth int)
	walkContainer = func(dir string, maxDepth, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := filepath.Join(dir, e.Name())
			found := tryAdd(child)
			if found || depth >= maxDepth || skipWalkDirs[e.Name()] {
				continue
			}
			walkContainer(child, maxDepth, depth+1)
		}
	}

	for _, dir := range priorityDirs {
		maxDepth := 1
		if deepDirs[dir] {
			maxDepth = defaultContainerDepth
		}
		walkContainer(dir, maxDepth, 1)
	}

	// Recursive fallback if nothing found or --full-depth is set.
	if len(out) == 0 || opts.FullDepth {
		var walkAll func(dir string, depth int)
		walkAll = func(dir string, depth int) {
			if depth > 5 {
				return
			}
			tryAdd(dir)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range entries {
				if e.IsDir() && !skipWalkDirs[e.Name()] {
					walkAll(filepath.Join(dir, e.Name()), depth+1)
				}
			}
		}
		walkAll(searchPath, 0)
	}

	return out, nil
}

func readSkillsShPaths(searchPath string) []string {
	b, err := os.ReadFile(filepath.Join(searchPath, "skills.sh.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	var dirs []string
	for _, p := range cfg.Paths {
		if p != "" && isSubpathSafe(searchPath, p) {
			dirs = append(dirs, filepath.Join(searchPath, filepath.FromSlash(p)))
		}
	}
	return dirs
}

func readPluginManifestSkillDirs(searchPath string) []string {
	var dirs []string
	for _, rel := range []string{".claude-plugin/marketplace.json", ".claude-plugin/plugin.json"} {
		b, err := os.ReadFile(filepath.Join(searchPath, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		var doc struct {
			Skills  []string `json:"skills"`
			Plugins []struct {
				Source string   `json:"source"`
				Skills []string `json:"skills"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		for _, s := range doc.Skills {
			if s != "" && isSubpathSafe(searchPath, s) {
				dirs = append(dirs, filepath.Join(searchPath, filepath.FromSlash(s)))
			}
		}
		for _, pl := range doc.Plugins {
			base := searchPath
			if pl.Source != "" && isSubpathSafe(searchPath, pl.Source) {
				base = filepath.Join(searchPath, filepath.FromSlash(pl.Source))
			}
			for _, s := range pl.Skills {
				if s != "" && isSubpathSafe(base, s) {
					dirs = append(dirs, filepath.Join(base, filepath.FromSlash(s)))
				}
			}
		}
	}
	return dirs
}

// FilterSkills matches skills by name (exact case-insensitive first, then
// normalized slug fallback). Passing "*" selects every skill.
func FilterSkills(skills []DiscoveredSkill, wanted []string) []DiscoveredSkill {
	if len(wanted) == 0 {
		return skills
	}
	if slices.Contains(wanted, "*") {
		return skills
	}
	selected := map[string]bool{}
	for _, input := range wanted {
		inLower := strings.ToLower(strings.TrimSpace(input))
		if inLower == "" {
			continue
		}
		var exact []string
		for _, s := range skills {
			if strings.ToLower(s.Name) == inLower {
				exact = append(exact, s.Name)
			}
		}
		if len(exact) > 0 {
			for _, n := range exact {
				selected[n] = true
			}
			continue
		}
		norm := NormalizeSkillName(input)
		for _, s := range skills {
			if NormalizeSkillName(s.Name) == norm {
				selected[s.Name] = true
			}
		}
	}
	var out []DiscoveredSkill
	for _, s := range skills {
		if selected[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// RenameSkill renames a single discovered skill in memory, rewriting SKILL.md's
// frontmatter name and recomputing its digests and content hash.
func RenameSkill(sk *DiscoveredSkill, newName string) error {
	if err := ValidateName(newName); err != nil {
		return err
	}
	for i := range sk.Files {
		if sk.Files[i].Path == "SKILL.md" {
			updated, err := RewriteFrontmatterName(sk.Files[i].Content, newName)
			if err != nil {
				return err
			}
			sk.Files[i].Content = updated
			sk.Files[i].Digest = SHA256Digest(updated)
			fm, _, err := ParseFrontmatter(updated)
			if err != nil {
				return err
			}
			fmJSON, err := FrontmatterJSON(fm)
			if err != nil {
				return err
			}
			sk.Name = newName
			sk.Frontmatter = fm
			sk.FrontmatterJSON = fmJSON
			sk.ContentHash = HashSkillFiles(sk.Files)
			return nil
		}
	}
	return fmt.Errorf("skill %q is missing SKILL.md", sk.Name)
}
