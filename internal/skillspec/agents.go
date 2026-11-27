package skillspec

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// AgentTarget describes a coding agent's project and global skill directories
// (matching vercel-labs/skills agent registry).
type AgentTarget struct {
	Name            string
	DisplayName     string
	ProjectDir      string // relative to project root
	GlobalDir       func(home string) string
	DetectInstalled func(home string) bool
}

func envOrJoin(envKey, home string, parts ...string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func xdgConfigHome(home string) string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return v
	}
	return filepath.Join(home, ".config")
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// KnownAgents lists the primary coding agents supported for local installation.
var KnownAgents = []AgentTarget{
	{
		Name:        "claude-code",
		DisplayName: "Claude Code",
		ProjectDir:  ".claude/skills",
		GlobalDir:   func(h string) string { return filepath.Join(envOrJoin("CLAUDE_CONFIG_DIR", h, ".claude"), "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(envOrJoin("CLAUDE_CONFIG_DIR", h, ".claude"))
		},
	},
	{
		Name:        "codex",
		DisplayName: "Codex",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(envOrJoin("CODEX_HOME", h, ".codex"), "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(envOrJoin("CODEX_HOME", h, ".codex"))
		},
	},
	{
		Name:        "cursor",
		DisplayName: "Cursor",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".cursor", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".cursor"))
		},
	},
	{
		Name:        "opencode",
		DisplayName: "OpenCode",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(xdgConfigHome(h), "opencode", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(xdgConfigHome(h), "opencode"))
		},
	},
	{
		Name:        "antigravity",
		DisplayName: "Antigravity",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".gemini", "config", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".gemini", "antigravity"))
		},
	},
	{
		Name:        "gemini-cli",
		DisplayName: "Gemini CLI",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".gemini", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".gemini"))
		},
	},
	{
		Name:        "github-copilot",
		DisplayName: "GitHub Copilot",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".copilot", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".copilot"))
		},
	},
	{
		Name:        "windsurf",
		DisplayName: "Windsurf",
		ProjectDir:  ".windsurf/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".codeium", "windsurf", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".codeium", "windsurf"))
		},
	},
	{
		Name:        "amp",
		DisplayName: "Amp",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(xdgConfigHome(h), "agents", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(xdgConfigHome(h), "amp"))
		},
	},
	{
		Name:        "goose",
		DisplayName: "Goose",
		ProjectDir:  ".goose/skills",
		GlobalDir:   func(h string) string { return filepath.Join(xdgConfigHome(h), "goose", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(xdgConfigHome(h), "goose"))
		},
	},
	{
		Name:        "cline",
		DisplayName: "Cline",
		ProjectDir:  ".agents/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".agents", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".cline"))
		},
	},
	{
		Name:        "roo",
		DisplayName: "Roo Code",
		ProjectDir:  ".roo/skills",
		GlobalDir:   func(h string) string { return filepath.Join(h, ".roo", "skills") },
		DetectInstalled: func(h string) bool {
			return pathExists(filepath.Join(h, ".roo"))
		},
	},
}

// ResolveAgents selects agent targets by name ("*" selects all known agents,
// empty auto-detects installed agents or falls back to claude-code).
func ResolveAgents(home string, requested []string) ([]AgentTarget, error) {
	if slices.Contains(requested, "*") {
		return slices.Clone(KnownAgents), nil
	}
	if len(requested) == 0 {
		var detected []AgentTarget
		for _, a := range KnownAgents {
			if a.DetectInstalled != nil && a.DetectInstalled(home) {
				detected = append(detected, a)
			}
		}
		return detected, nil
	}
	byName := make(map[string]AgentTarget, len(KnownAgents))
	for _, a := range KnownAgents {
		byName[a.Name] = a
	}
	var out []AgentTarget
	seen := map[string]bool{}
	for _, name := range requested {
		n := strings.ToLower(strings.TrimSpace(name))
		a, ok := byName[n]
		if !ok {
			var valid []string
			for _, ka := range KnownAgents {
				valid = append(valid, ka.Name)
			}
			return nil, fmt.Errorf("unknown agent %q (supported: %s, or '*')", name, strings.Join(valid, ", "))
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, a)
		}
	}
	return out, nil
}

// LocalInstallOptions controls installing a skill onto disk for local coding agents.
type LocalInstallOptions struct {
	Global bool
	Copy   bool
	Cwd    string
	Home   string
	Agents []AgentTarget
}

// LocalInstallResult describes where a skill was materialized on disk.
type LocalInstallResult struct {
	SkillName    string   `json:"skill"`
	CanonicalDir string   `json:"canonicalDir"`
	AgentDirs    []string `json:"agentDirs,omitempty"`
	Mode         string   `json:"mode"` // "symlink" or "copy"
	Files        int      `json:"files"`
}

// InstallToAgents writes a skill into the canonical `.agents/skills/<name>` (or
// `~/.agents/skills/<name>` with Global) and symlinks or copies it into each
// target agent's skills directory, matching `npx skills` installation semantics.
func InstallToAgents(skillName string, files []FileEntry, opts LocalInstallOptions) (*LocalInstallResult, error) {
	if err := ValidateName(skillName); err != nil {
		return nil, err
	}
	home := opts.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}

	var canonicalBase string
	if opts.Global {
		canonicalBase = filepath.Join(home, ".agents", "skills")
	} else {
		canonicalBase = filepath.Join(cwd, ".agents", "skills")
	}
	canonicalDir := filepath.Join(canonicalBase, skillName)
	if err := writeSkillTree(canonicalDir, files); err != nil {
		return nil, err
	}

	mode := "symlink"
	if opts.Copy {
		mode = "copy"
	}
	res := &LocalInstallResult{
		SkillName:    skillName,
		CanonicalDir: canonicalDir,
		Mode:         mode,
		Files:        len(files),
	}

	seenDirs := map[string]bool{filepath.Clean(canonicalDir): true}
	for _, agent := range opts.Agents {
		var base string
		if opts.Global {
			base = agent.GlobalDir(home)
		} else {
			base = filepath.Join(cwd, filepath.FromSlash(agent.ProjectDir))
		}
		targetDir := filepath.Clean(filepath.Join(base, skillName))
		if seenDirs[targetDir] {
			continue
		}
		seenDirs[targetDir] = true
		if err := os.MkdirAll(filepath.Dir(targetDir), 0o755); err != nil {
			return nil, err
		}
		if err := os.RemoveAll(targetDir); err != nil {
			return nil, err
		}
		if opts.Copy {
			if err := writeSkillTree(targetDir, files); err != nil {
				return nil, err
			}
		} else {
			rel, err := filepath.Rel(filepath.Dir(targetDir), canonicalDir)
			if err != nil {
				rel = canonicalDir
			}
			if err := os.Symlink(rel, targetDir); err != nil {
				// Fall back to copy if symlinks are unsupported on this filesystem.
				if cerr := writeSkillTree(targetDir, files); cerr != nil {
					return nil, cerr
				}
			}
		}
		res.AgentDirs = append(res.AgentDirs, targetDir)
	}
	return res, nil
}

func writeSkillTree(dstDir string, files []FileEntry) error {
	if err := os.RemoveAll(dstDir); err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		clean, err := sanitizeArchivePath(f.Path)
		if err != nil || clean == "" {
			return fmt.Errorf("invalid skill file path %q", f.Path)
		}
		full := filepath.Join(dstDir, filepath.FromSlash(clean))
		if !isSubpathSafe(dstDir, clean) {
			return fmt.Errorf("skill file path %q escapes target dir", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			return err
		}
	}
	return nil
}
