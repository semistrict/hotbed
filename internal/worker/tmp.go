package worker

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Each command has a temporary directory of its own, its TMPDIR: on disk,
// not in a tmpfs /tmp, whose files are memory charged to whoever wrote them
// and kept past their end (killed tests' stores of several GB filled a
// fleet host's swap). It goes when the command ends.

// tmpRoot holds the commands' temporary directories: the worker's tmp-dir
// (such as an ext4 scratch disk), or its state directory's tmp.
func (e *Engine) tmpRoot() string { return cmp.Or(e.cfg.TmpDir, filepath.Join(e.cfg.Dir, "tmp")) }

// tmpDir is command id's. Short: tests put sockets in it, and a socket's
// path has room for 108 bytes at most.
func (e *Engine) tmpDir(id string) string { return filepath.Join(e.tmpRoot(), id) }

// makeTmp makes command id's temporary directory, anyone's to write in, as
// /tmp is: a test may run a VM as another user that writes there too.
func (e *Engine) makeTmp(id string) error {
	if err := os.MkdirAll(e.tmpRoot(), 0o711); err != nil {
		return err
	}
	dir := e.tmpDir(id)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Chmod(dir, fs.ModePerm|fs.ModeSticky)
}

// removeTmp removes command id's temporary directory, which nothing of it
// runs in any more. What is still mounted in it, such as a killed FUSE
// test's mount, is let go of first, deepest first; while anything is, the
// directory stays, as removing it would go on into the mount.
func (e *Engine) removeTmp(id string) {
	dir := e.tmpDir(id)
	for range 10 {
		targets := mountsUnder(dir)
		if len(targets) == 0 {
			break
		}
		for _, t := range targets {
			if err := detach(t); err != nil {
				slog.Warn("unmount in a command's TMPDIR", "command", id, "mount", t, "err", err)
			}
		}
	}
	if left := mountsUnder(dir); len(left) > 0 {
		slog.Error("TMPDIR stays, still mounted", "command", id, "mounts", left)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Error("remove a command's TMPDIR", "command", id, "err", err)
	}
}

// sweepTmp removes the temporary directories of commands that are not
// live: those a worker that stopped meanwhile left.
func (e *Engine) sweepTmp() {
	entries, err := os.ReadDir(e.tmpRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Warn("sweep TMPDIRs", "err", err)
		return
	}
	e.mu.Lock()
	var dead []string
	for _, ent := range entries {
		if c, ok := e.commands[ent.Name()]; !ok || !live(c.cmd().State) {
			dead = append(dead, ent.Name())
		}
	}
	e.mu.Unlock()
	for _, id := range dead {
		e.removeTmp(id)
	}
}

// mountsUnder lists what is mounted at or below dir, deepest first, from
// /proc/self/mountinfo; none where there is no such file.
func mountsUnder(dir string) []string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMountsUnder(b, dir)
}

func parseMountsUnder(mountinfo []byte, dir string) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		target := unescapeMount(f[4])
		if target == dir || strings.HasPrefix(target, dir+"/") {
			out = append(out, target)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
