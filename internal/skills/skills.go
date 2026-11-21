// Package skills holds the guides for agents that use hotbed, compiled into
// the binary so they always match it.
package skills

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Skill is one guide.
type Skill struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

var summaries = map[string]string{
	"hotbed": "Core guide: what a cluster is, setting one up, the JSON output and its error codes. Read this first.",
}

// List returns every guide.
func List() []Skill {
	var out []Skill
	for name, summary := range summaries {
		out = append(out, Skill{Name: name, Summary: summary})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the guide called name, and whether there is one.
func Get(name string) (string, bool) {
	name = strings.TrimSuffix(name, ".md")
	if _, ok := summaries[name]; !ok {
		return "", false
	}
	b, err := fs.ReadFile(files, name+".md")
	if err != nil {
		// Every name in summaries has a file; TestEverySkillHasAFile checks it.
		return "", false
	}
	return string(b), true
}
