package worker

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/semistrict/hotbed/internal/api"
)

// Each command has a temporary directory of its own on its workspace's
// volume, which it sees as /tmp and /var/tmp, with TMPDIR=/tmp. Not the
// host's tmpfs /tmp, whose files are memory charged to whoever wrote them
// and kept past their end (killed tests' stores of several GB filled a
// fleet host's swap), and shared with every other command. On the volume, it
// is the workspace's disk. It goes when the command ends, before the volume
// is snapshotted as a base or destroyed.

// tmpName holds a volume's commands' temporary directories, in its root
// beside its checkouts: no repo id begins with a dot.
const tmpName = ".tmp"

func (e *Engine) tmpRoot(vol string) string { return filepath.Join(e.cfg.Store.Path(vol), tmpName) }

// tmpDir is c's temporary directory, bound to /tmp and /var/tmp.
func (e *Engine) tmpDir(c *command) (string, error) {
	if err := validateID(c.cmd().Id); err != nil {
		return "", err
	}
	return filepath.Join(e.tmpRoot(c.rec.Volume), c.cmd().Id, "tmp"), nil
}

// runtimeDir is c's per-command XDG_RUNTIME_DIR backing directory, bound to
// /run/user/<uid>. Keeping both tmp and run under .tmp/<id> lets removeTmp,
// dropTmp, sweepTmp, and unmount cleanup manage them as one unit.
func (e *Engine) runtimeDir(c *command) (string, error) {
	if err := validateID(c.cmd().Id); err != nil {
		return "", err
	}
	return filepath.Join(e.tmpRoot(c.rec.Volume), c.cmd().Id, "run"), nil
}

// makeTmpEntry creates <volPath>/.tmp/<name>/{tmp,run} without following
// symlinks. Callers that need the build user to own run pass cred to
// makeTmpDirs.
func makeTmpEntry(volPath, name string) error {
	return makeTmpDirs(volPath, name, nil)
}

// makeTmpDirs creates <volPath>/.tmp/<name> (0711) containing tmp (01777, for
// /tmp and /var/tmp) and run (0700, owned by cred when running as root, for
// XDG_RUNTIME_DIR) without following symlinks (including in-root symlinks).
func makeTmpDirs(volPath, name string, cred *syscall.Credential) error {
	if err := validateID(name); err != nil {
		return err
	}
	volRoot, err := os.OpenRoot(volPath)
	if err != nil {
		return err
	}
	defer volRoot.Close()
	if err := volRoot.Mkdir(tmpName, 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmpFi, err := volRoot.Lstat(tmpName)
	if err != nil {
		return err
	}
	if tmpFi.Mode()&os.ModeSymlink != 0 || !tmpFi.IsDir() {
		return fmt.Errorf("%s in %s is not a directory", tmpName, volPath)
	}
	tmpRoot, err := volRoot.OpenRoot(tmpName)
	if err != nil {
		return err
	}
	defer tmpRoot.Close()
	if dotFi, err := tmpRoot.Stat("."); err != nil {
		return err
	} else if !os.SameFile(tmpFi, dotFi) {
		return fmt.Errorf("%s in %s changed while opening", tmpName, volPath)
	}
	if err := ensureConfinedDir(tmpRoot, name, 0o711, nil); err != nil {
		return err
	}
	entryRoot, err := tmpRoot.OpenRoot(name)
	if err != nil {
		return err
	}
	defer entryRoot.Close()
	if err := ensureConfinedDir(entryRoot, "tmp", fs.ModePerm|fs.ModeSticky, nil); err != nil {
		return err
	}
	return ensureConfinedDir(entryRoot, "run", 0o700, cred)
}

// ensureConfinedDir creates or verifies a real directory named name inside
// root without following symlinks or blocking on FIFOs, applies mode, and
// chowns it to cred when non-nil and running as root.
func ensureConfinedDir(root *os.Root, name string, mode fs.FileMode, cred *syscall.Credential) error {
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	lfi, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("tmp entry %q is a symlink", name)
	}
	if !lfi.IsDir() {
		return fmt.Errorf("tmp entry %q is not a directory", name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	ffi, err := f.Stat()
	if err != nil {
		return err
	}
	if !ffi.IsDir() || !os.SameFile(lfi, ffi) {
		return fmt.Errorf("tmp entry %q is not a directory", name)
	}
	if cred != nil && os.Geteuid() == 0 {
		if err := f.Chown(int(cred.Uid), int(cred.Gid)); err != nil {
			return err
		}
	}
	return f.Chmod(mode)
}

// makeTmp makes c's per-command temporary and XDG runtime directories.
func (e *Engine) makeTmp(c *command) error {
	return makeTmpDirs(e.cfg.Store.Path(c.rec.Volume), c.cmd().Id, e.cred)
}

// dropTmp removes c's temporary directory, its processes gone, in the
// background; c.tmpGone closes once it has. When the engine is shutting down
// (startBgLocked returns false), dropTmp closes c.tmpGone immediately rather
// than blocking shutdown—the next startup's sweepTmp cleans up any leftover
// .tmp entry. Call with e.mu held.
func (e *Engine) dropTmp(c *command) {
	gone := make(chan struct{})
	c.tmpGone = gone
	if _, err := e.tmpDir(c); err != nil {
		slog.Error("skip tmp cleanup for invalid command ID", "command", c.cmd().Id, "err", err)
		close(gone)
		return
	}
	volPath := e.cfg.Store.Path(c.rec.Volume)
	id := c.cmd().Id
	admitDone := c.admitDone
	if !e.startBgLocked(func(context.Context) {
		defer close(gone)
		if admitDone != nil {
			<-admitDone
		}
		removeTmp(volPath, id)
	}) {
		close(gone)
	}
}

// tmpsGone waits until the temporary directories of workspace id's ended
// commands are removed, none left in its volume.
func (e *Engine) tmpsGone(ctx context.Context, id string) error {
	e.mu.Lock()
	var gone []chan struct{}
	for _, c := range e.commands {
		if c.cmd().WorkspaceId == id && c.tmpGone != nil {
			gone = append(gone, c.tmpGone)
		}
	}
	e.mu.Unlock()
	for _, g := range gone {
		select {
		case <-g:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// openTmpRoot opens <volPath>/.tmp confined inside volPath, refusing to follow
// any symlink at .tmp. Returns (nil, nil) when volPath or .tmp does not exist.
func openTmpRoot(volPath string) (*os.Root, error) {
	volRoot, err := os.OpenRoot(volPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer volRoot.Close()
	fi, err := volRoot.Lstat(tmpName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("%s in %s is not a directory", tmpName, volPath)
	}
	tmpRoot, err := volRoot.OpenRoot(tmpName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dotFi, err := tmpRoot.Stat(".")
	if err != nil {
		tmpRoot.Close()
		return nil, err
	}
	if !os.SameFile(fi, dotFi) {
		tmpRoot.Close()
		return nil, fmt.Errorf("%s in %s changed while opening", tmpName, volPath)
	}
	return tmpRoot, nil
}

// removeTmp removes a command's temporary directory inside volPath/.tmp, which
// nothing of it runs in any more. It anchors resolution at volPath via
// os.OpenRoot so a symlink at .tmp or <id> can never cause deletion outside
// the volume.
func removeTmp(volPath, id string) {
	tmpRoot, err := openTmpRoot(volPath)
	if err != nil {
		slog.Error("open tmp root for removal", "volume", volPath, "command", id, "err", err)
		return
	}
	if tmpRoot == nil {
		return
	}
	defer tmpRoot.Close()
	removeTmpFromRoot(tmpRoot, filepath.Join(volPath, tmpName), id)
}

// removeTmpFromRoot removes id from an already-anchored tmpRoot. What is still
// mounted in a real directory entry, such as a killed FUSE test's mount, is let
// go of first, deepest first; while anything is, the directory stays, as
// removing it would go on into the mount.
func removeTmpFromRoot(tmpRoot *os.Root, tmpHostDir, id string) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		slog.Error("skip tmp cleanup for invalid entry name", "entry", id)
		return
	}
	fi, err := tmpRoot.Lstat(id)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Error("stat a command's tmp", "command", id, "err", err)
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// Unlink the symlink entry itself without traversing its target.
		if err := tmpRoot.Remove(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Error("remove symlinked command tmp entry", "command", id, "err", err)
		}
		return
	}
	if fi.IsDir() {
		dir := filepath.Join(tmpHostDir, id)
		for range 10 {
			targets := mountsUnder(dir)
			if len(targets) == 0 {
				break
			}
			for _, t := range targets {
				if err := detach(t); err != nil {
					slog.Warn("unmount in a command's tmp", "command", id, "mount", t, "err", err)
				}
			}
		}
		if left := mountsUnder(dir); len(left) > 0 {
			slog.Error("a command's tmp stays, still mounted", "command", id, "mounts", left)
			return
		}
	}
	if err := tmpRoot.RemoveAll(id); err != nil {
		slog.Error("remove a command's tmp", "command", id, "err", err)
	}
}

// sweepTmp removes the temporary directories of commands that are not
// live from every volume: those a worker that stopped meanwhile left, and
// any a base's snapshot kept.
func (e *Engine) sweepTmp() {
	e.mu.Lock()
	live := map[string]bool{}
	for _, c := range e.commands {
		if api.Live(c.cmd().State) {
			live[c.cmd().Id] = true
		}
	}
	vols := map[string]bool{}
	for _, b := range e.bases {
		vols[baseVolume(b.Project, b.Generation)] = true
	}
	for _, w := range e.workspaces {
		if w.ws().State != released {
			vols[w.rec.Volume] = true
		}
	}
	e.mu.Unlock()
	for vol := range vols {
		sweepVolTmp(vol, e.cfg.Store.Path(vol), live)
	}
}

func sweepVolTmp(vol, volPath string, live map[string]bool) {
	tmpRoot, err := openTmpRoot(volPath)
	if err != nil {
		slog.Warn("sweep tmp", "volume", vol, "err", err)
		return
	}
	if tmpRoot == nil {
		return
	}
	defer tmpRoot.Close()
	dirFile, err := tmpRoot.Open(".")
	if err != nil {
		slog.Warn("sweep tmp", "volume", vol, "err", err)
		return
	}
	names, err := dirFile.Readdirnames(-1)
	dirFile.Close()
	if err != nil {
		slog.Warn("sweep tmp", "volume", vol, "err", err)
		return
	}
	tmpHostDir := filepath.Join(volPath, tmpName)
	for _, name := range names {
		if !live[name] {
			removeTmpFromRoot(tmpRoot, tmpHostDir, name)
		}
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
