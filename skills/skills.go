// Package skills holds the guides for agents that use hotbed, in the Agent
// Skills layout: one directory per skill with a SKILL.md. They live in the
// repo (where agents working on hotbed find them through .claude/skills),
// are compiled into the binary (hotbed skills get), and install into an
// agent's skills directory (hotbed skills install).
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:generate go run ../cmd/gen-skills

//go:embed */SKILL.md */references
var files embed.FS

// Skill is one guide.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	References  []string `json:"references,omitempty"`
}

// List returns every skill, by name.
func List() ([]Skill, error) {
	dirs, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		s, err := read(d.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func read(name string) (Skill, error) {
	b, err := fs.ReadFile(files, name+"/SKILL.md")
	if err != nil {
		return Skill{}, err
	}
	s := Skill{Name: name, Description: frontMatter(string(b), "description")}
	refs, err := fs.ReadDir(files, name+"/references")
	if err == nil {
		for _, r := range refs {
			s.References = append(s.References, "references/"+r.Name())
		}
	}
	return s, nil
}

// frontMatter reads one key of a SKILL.md's YAML front matter.
func frontMatter(doc, key string) string {
	rest, ok := strings.CutPrefix(doc, "---\n")
	if !ok {
		return ""
	}
	head, _, _ := strings.Cut(rest, "\n---")
	for _, line := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// IsBuiltin says whether name is one of these skills.
func IsBuiltin(name string) bool {
	if name == "" || strings.ContainsAny(name, "/.") {
		return false
	}
	_, err := fs.Stat(files, name+"/SKILL.md")
	return err == nil
}

// Get returns a skill's SKILL.md, or one of its files ("hotbed/references/x.md").
func Get(path string) (string, error) {
	path = strings.TrimSuffix(path, "/")
	if !strings.Contains(path, "/") {
		path += "/SKILL.md"
	}
	b, err := fs.ReadFile(files, path)
	if err != nil {
		return "", fmt.Errorf("no skill file %s", path)
	}
	return string(b), nil
}

// ReadAll returns every embedded file for the named built-in skill, keyed by
// relative POSIX path ("SKILL.md", "references/commands.md", ...).
func ReadAll(name string) (map[string][]byte, error) {
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("no built-in skill %q", name)
	}
	out := map[string][]byte{}
	err := fs.WalkDir(files, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, name+"/")
		out[rel] = b
		return nil
	})
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("no built-in skill %q", name)
	}
	return out, nil
}

// Install writes every skill into dir (an agent's skills directory, such as
// ~/.claude/skills), replacing older copies, and returns what it wrote.
func Install(dir string) ([]string, error) {
	var wrote []string
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		wrote = append(wrote, dst)
		return nil
	})
	return wrote, err
}
