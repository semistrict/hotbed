//go:build linux

package worker

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/sysinfo"
	"golang.org/x/sys/unix"
)

// clearMountNosuid clears MOUNT_ATTR_NOSUID on dir when it is mounted nosuid
// (e.g. a cladefsd host FUSE mount created via fusermount3), so setuid binaries
// such as sudo work inside chrooted workspaces and setup steps.
func clearMountNosuid(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Flags&unix.ST_NOSUID == 0 {
		return nil
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, dir, 0, &unix.MountAttr{Attr_clr: unix.MOUNT_ATTR_NOSUID}); err != nil {
		return fmt.Errorf("clear nosuid on %s: %w", dir, err)
	}
	return nil
}

// cleanupFUSEMountDir prepares a host-FUSE cell directory for MountDetach:
// it terminates any lingering non-self processes still referencing mntDir
// (via root, cwd, exe, or open FDs), detaches any leftover submounts strictly
// beneath mntDir, and optionally calls syncfs on mntDir to flush dirty kernel
// buffers before cladefsd unmounts the cell.
func cleanupFUSEMountDir(mntDir string, syncFS bool) {
	mntDir = filepath.Clean(mntDir)
	if !filepath.IsAbs(mntDir) || mntDir == "/" {
		return
	}
	terminateMountUsers("/proc", mntDir)
	unmountSubmountsUnder(mntDir)
	if syncFS {
		fd, err := unix.Open(mntDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == nil {
			if serr := unix.Syncfs(fd); serr != nil && serr != unix.ENOSYS {
				slog.Warn("syncfs cladefs FUSE mount dir before detach", "dir", mntDir, "err", serr)
			}
			_ = unix.Close(fd)
		}
	}
}

func pathMatchesOrInside(target, dir string) bool {
	target = strings.TrimSuffix(target, " (deleted)")
	return target == dir || strings.HasPrefix(target, dir+"/")
}

func procUsesMountDir(procRoot, mntDir string, pid int) bool {
	pdir := filepath.Join(procRoot, strconv.Itoa(pid))
	for _, link := range []string{"root", "cwd", "exe"} {
		if target, err := os.Readlink(filepath.Join(pdir, link)); err == nil && pathMatchesOrInside(target, mntDir) {
			return true
		}
	}
	fds, err := os.ReadDir(filepath.Join(pdir, "fd"))
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join(pdir, "fd", fd.Name())); err == nil && pathMatchesOrInside(target, mntDir) {
			return true
		}
	}
	return false
}

func findMountUsers(procRoot, mntDir string, selfPID int) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}
		if procUsesMountDir(procRoot, mntDir, pid) {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
}

func terminateMountUsers(procRoot, mntDir string) {
	selfPID := os.Getpid()
	pids := findMountUsers(procRoot, mntDir, selfPID)
	if len(pids) == 0 {
		return
	}
	slog.Warn("terminating lingering processes referencing cladefs FUSE mount before detach",
		"dir", mntDir, "pids", pids)
	for _, pid := range pids {
		_ = unix.Kill(pid, unix.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pids = findMountUsers(procRoot, mntDir, selfPID)
		if len(pids) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	pids = findMountUsers(procRoot, mntDir, selfPID)
	if len(pids) == 0 {
		return
	}
	slog.Warn("sending SIGKILL to lingering processes referencing cladefs FUSE mount",
		"dir", mntDir, "pids", pids)
	for _, pid := range pids {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
	killDeadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(killDeadline) {
		if len(findMountUsers(procRoot, mntDir, selfPID)) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func unmountSubmountsUnder(mntDir string) {
	mounts, err := sysinfo.MountsUnder(mntDir, sysinfo.Beneath)
	if err != nil {
		slog.Warn("read the submounts under a cladefs FUSE cell dir", "dir", mntDir, "err", err)
		return
	}
	for _, mp := range mounts {
		slog.Warn("detaching leftover submount under cladefs FUSE cell dir", "dir", mntDir, "submount", mp)
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
			slog.Warn("detach leftover submount failed", "submount", mp, "err", err)
		}
	}
}

// fuseSuperMagic is Linux's FUSE_SUPER_MAGIC (include/uapi/linux/magic.h).
const fuseSuperMagic uint64 = 0x65735546

// checkMountHealth verifies that dir is an active, responsive FUSE mount via
// an uncached statfs(2) syscall, catching both dead FUSE mounts (ENOTCONN) and
// unmounted mountpoints while cladefsd is down or restarting.
func checkMountHealth(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if uint64(st.Type) != fuseSuperMagic {
		return fmt.Errorf("%s is not a FUSE mount (fs type 0x%x)", dir, uint64(st.Type))
	}
	return nil
}

func isMountNotConnected(err error) bool {
	return errors.Is(err, unix.ENOTCONN)
}

// detachDeadMount lazily unmounts dir if and only if statfs(2) reports
// ENOTCONN (a dead FUSE mount whose daemon exited or was OOM-killed), allowing
// cladefsd to remount cleanly instead of crash-looping on fusermount3.
func detachDeadMount(dir string) error {
	var st unix.Statfs_t
	err := unix.Statfs(dir, &st)
	if !errors.Is(err, unix.ENOTCONN) {
		return nil
	}
	if uerr := unix.Unmount(dir, unix.MNT_DETACH); uerr != nil && !errors.Is(uerr, unix.EINVAL) && !errors.Is(uerr, unix.ENOENT) {
		return fmt.Errorf("lazy unmount dead FUSE mount %s: %w", dir, uerr)
	}
	return nil
}
