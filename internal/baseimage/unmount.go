package baseimage

import (
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/sysinfo"
)

// parseSubmounts parses Linux /proc/self/mountinfo content and returns all
// mountpoints strictly underneath rootDir, ordered deepest-first so nested
// mounts (e.g. <root>/dev/pts) are unmounted before their parents (<root>/dev).
// rootDir is the worker's own (a base image's volume), so a symlink in it is
// followed: mountinfo names the mounts by their real paths.
func parseSubmounts(mountinfo, rootDir string) []string {
	if rootDir == "" {
		return nil
	}
	root := filepath.Clean(rootDir)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	// Reading a string cannot fail.
	mounts, _ := sysinfo.ParseMountsUnder(strings.NewReader(mountinfo), root, sysinfo.Beneath)
	return mounts
}
