package skills

import "testing"

func TestEverySkillHasAFile(t *testing.T) {
	for _, s := range List() {
		if _, ok := Get(s.Name); !ok {
			t.Errorf("skill %q has a summary but no %s.md", s.Name, s.Name)
		}
	}
}
