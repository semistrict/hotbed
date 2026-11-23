package skills

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1cli"
)

func TestGeneratedCommandsReferenceIsUpToDate(t *testing.T) {
	s, err := spec.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	want := apicmd.RenderCommandReference(s)
	got, err := Get("hotbed/references/commands.md")
	if err != nil {
		t.Fatalf("missing generated reference: %v (run `go generate ./skills`)", err)
	}
	if got != want {
		t.Fatalf("skills/hotbed/references/commands.md is out of date with hotbed.proto; run `go generate ./skills`")
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)]+)\)`)

func TestSkillInternalLinksResolve(t *testing.T) {
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		dir := path.Dir(p)
		for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "#") {
				continue
			}
			filePart, _, _ := strings.Cut(target, "#")
			if filePart == "" {
				continue
			}
			resolved := path.Clean(path.Join(dir, filePart))
			if _, err := fs.Stat(files, resolved); err != nil {
				t.Errorf("%s: broken link %q (resolved to %s)", p, target, resolved)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
