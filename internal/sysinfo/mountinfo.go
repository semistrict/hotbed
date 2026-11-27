package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// MountScope says whether the mounts under a directory include one on the
// directory itself.
type MountScope int

const (
	// Beneath is the mounts strictly below the directory: those to take
	// down before the directory's own mount, which stays.
	Beneath MountScope = iota
	// AtOrBeneath is those and one on the directory itself.
	AtOrBeneath
)

// MountsUnder lists what is mounted under dir (within scope), from
// /proc/self/mountinfo, deepest first; none where there is no such file
// (not Linux).
func MountsUnder(dir string, scope MountScope) ([]string, error) {
	f, err := os.Open(path("/proc/self/mountinfo"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountsUnder(f, dir, scope)
}

// ParseMountsUnder lists the mount points in mountinfo (/proc/self/mountinfo's
// format) under dir, within scope: each once, cleaned, deepest first, so one
// can be taken down before what it is mounted on. dir must be absolute and is
// matched as written: a caller that wants a symlink in it followed resolves
// it first, and only where it trusts every component (one an agent could
// swap would aim the unmounts at another tree). No dir holds "/": that would
// be every mount on the machine.
func ParseMountsUnder(mountinfo io.Reader, dir string, scope MountScope) ([]string, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) || dir == "/" {
		return nil, nil
	}
	prefix := dir + "/"
	seen := map[string]bool{}
	var out []string
	// A bufio.Reader, not a Scanner: a line has no length limit.
	r := bufio.NewReader(mountinfo)
	for {
		line, err := r.ReadBytes('\n')
		if f := bytes.Fields(line); len(f) >= 5 {
			mp := filepath.Clean(UnescapeMountinfoPath(string(f[4])))
			if (strings.HasPrefix(mp, prefix) || (scope == AtOrBeneath && mp == dir)) && !seen[mp] {
				seen[mp] = true
				out = append(out, mp)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
			return db - da
		}
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(b, a)
	})
	return out, nil
}

// UnescapeMountinfoPath undoes the octal escapes (\040 for a space, \011,
// \012, \134) the kernel writes in mountinfo's paths.
func UnescapeMountinfoPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] <= '3' && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
