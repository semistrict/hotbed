package inspect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var chrootBinDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

const chrootExecScript = `root="$1"; tmp="$2"; shift 2
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export HOME="/nonexistent"
export XDG_CONFIG_HOME="/nonexistent"
mount --rbind /proc "$root/proc"
mount --rbind /sys "$root/sys"
mount --rbind /dev "$root/dev"
if [ -n "$tmp" ]; then
  mount --bind "$tmp" "$root/tmp"
fi
exec chroot "$root" "$@"`

// lookToolInRoot locates an executable binary named name. When rootDir is
// empty it searches the host PATH via exec.LookPath; when rootDir is set it
// searches standard bin directories inside rootDir (following symlinks
// confined to rootDir) and returns the in-chroot absolute path (e.g.
// "/usr/bin/strace").
func lookToolInRoot(rootDir, name string) (string, error) {
	if rootDir == "" {
		return exec.LookPath(name)
	}
	if name == "" || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("invalid tool name %q", name)
	}
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", err
	}
	for _, dir := range chrootBinDirs {
		inRoot := dir + "/" + name
		if isExecutableInRoot(rootAbs, inRoot) {
			return inRoot, nil
		}
	}
	return "", fmt.Errorf("%s not found in rootfs %s", name, rootDir)
}

func isExecutableInRoot(rootAbs, inRoot string) bool {
	cur := filepath.Clean("/" + inRoot)
	for range 16 {
		rel := strings.TrimPrefix(cur, "/")
		hostPath := filepath.Join(rootAbs, rel)
		r, err := filepath.Rel(rootAbs, hostPath)
		if err != nil || r == ".." || strings.HasPrefix(r, "../") {
			return false
		}
		fi, err := os.Lstat(hostPath)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(hostPath)
			if err != nil {
				return false
			}
			if strings.HasPrefix(target, "/") {
				cur = filepath.Clean(target)
			} else {
				cur = filepath.Clean(filepath.Join(filepath.Dir(cur), target))
			}
			continue
		}
		return fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
	}
	return false
}

// chrootCmd wraps (bin, args) so that when rootDir is non-empty it runs inside
// a private mount namespace rooted at rootDir with host /proc, /sys (including
// tracefs, BTF, and cgroup2), and /dev rbind-mounted—keeping the host PID
// namespace so tracers can attach to live command PIDs without requiring any
// tracer packages on the worker host. If tmpBindHost is non-empty it is
// bind-mounted onto /tmp inside the chroot.
func chrootCmd(rootDir, tmpBindHost, bin string, args []string) (string, []string) {
	if rootDir == "" {
		return bin, args
	}
	wrapped := make([]string, 0, 10+len(args))
	wrapped = append(wrapped,
		"--mount", "--propagation", "slave", "--",
		"/bin/sh", "-eu", "-c", chrootExecScript, "hotbed-trace",
		rootDir, tmpBindHost, bin,
	)
	wrapped = append(wrapped, args...)
	return "unshare", wrapped
}
