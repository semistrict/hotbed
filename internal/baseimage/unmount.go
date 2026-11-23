package baseimage

import (
	"path/filepath"
	"slices"
	"strings"
)

// parseSubmounts parses Linux /proc/self/mountinfo content and returns all
// mountpoints strictly underneath rootDir, ordered deepest-first so nested
// mounts (e.g. <root>/dev/pts) are unmounted before their parents (<root>/dev).
func parseSubmounts(mountinfo, rootDir string) []string {
	if rootDir == "" {
		return nil
	}
	root := filepath.Clean(rootDir)
	if root == "/" || root == "." {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = filepath.Clean(resolved)
		if root == "/" || root == "." {
			return nil
		}
	}
	prefix := root + "/"
	var mounts []string
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := filepath.Clean(unescapeMountinfoPath(fields[4]))
		if strings.HasPrefix(mp, prefix) {
			mounts = append(mounts, mp)
		}
	}
	slices.SortFunc(mounts, func(a, b string) int {
		if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
			return db - da
		}
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(b, a)
	})
	return mounts
}

// unescapeMountinfoPath decodes octal escapes (\040, \011, \012, \134, etc.)
// emitted by the Linux kernel in /proc/self/mountinfo mountpoint fields.
func unescapeMountinfoPath(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) && isOctalDigit(s[i+1]) && isOctalDigit(s[i+2]) && isOctalDigit(s[i+3]) {
			v := (s[i+1]-'0')*64 + (s[i+2]-'0')*8 + (s[i+3] - '0')
			b.WriteByte(v)
			i += 4
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }
