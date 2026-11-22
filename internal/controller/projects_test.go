package controller

import "testing"

func TestProjectNameFor(t *testing.T) {
	for remote, want := range map[string]string{
		"github.com/semistrict/hotbed": "hotbed",
		"github.com/vercel/Next.js":    "next-js",
		"github.com/o/my_repo":         "my-repo",
		"github.com/o/MyApp":           "myapp",
		"github.com/o/dotfiles.nvim":   "dotfiles-nvim",
		"github.com/o/__":              "",
		"github.com/o/" + "a234567890b234567890c234567890d234567890-e": "a234567890b234567890c234567890d234567890",
		"/srv/git/Tools-": "tools",
	} {
		if got := projectNameFor(remote); got != want {
			t.Errorf("projectNameFor(%q) = %q, want %q", remote, got, want)
		}
	}
}
