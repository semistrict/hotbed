package unit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/pty"
)

// ShellIO configures stdio for an interactive or piped shell process.
type ShellIO struct {
	// PTY requests a pseudo-terminal with the given initial dimensions and TERM.
	PTY  bool
	Cols uint16
	Rows uint16
	Term string
	// Stdin, Stdout, Stderr are used when PTY is false.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ShellHandle is a running interactive shell process (PTY or piped).
type ShellHandle interface {
	// PTY returns the master PTY file when started with PTY=true, else nil.
	PTY() *os.File
	// Resize updates the PTY window size (no-op when PTY is nil).
	Resize(cols, rows uint16) error
	// Wait waits for the shell process to exit and returns its exit code
	// (128+signal when terminated by a signal).
	Wait() int
	// Close terminates the shell and every process in its cgroup/process group.
	Close() error
}

// shellOOMScoreAdj gives a shell or exec scope, and all it starts, the
// oom_score_adj of a command (OOMScoreAdjust). A scope runs no ExecStart of
// systemd's, so systemd's OOMScoreAdjust= is not for it.
var shellOOMScoreAdj = "echo " + strconv.Itoa(OOMScoreAdjust) + " > /proc/self/oom_score_adj\n"

// chrootScript sets up a private mount namespace rooted at root, binds API VFS
// and requested paths, restores the target PATH, and execs chroot into dir.
// Every path and argument arrives through positional parameters so nothing is
// interpolated into shell syntax.
const chrootScript = `target_path="${PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}"
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
root="$1"; dir="$2"; userspec="$3"; groups="$4"; rw_count="$5"; ro_count="$6"
shift 6
mount --rbind /proc "$root/proc"
mount --rbind /sys "$root/sys"
mount --rbind /dev "$root/dev"
if [ -e /etc/resolv.conf ]; then
  mkdir -p "$root/etc"
  [ -f "$root/etc/resolv.conf" ] && [ ! -L "$root/etc/resolv.conf" ] || { rm -f "$root/etc/resolv.conf"; : > "$root/etc/resolv.conf"; }
  mount --bind /etc/resolv.conf "$root/etc/resolv.conf"
  mount -o remount,ro,bind "$root/etc/resolv.conf" 2>/dev/null || true
fi
bind_one() {
  src="$1"; dst="$2"; ro="$3"
  if [ -d "$src" ]; then
    mkdir -p "$root$dst"
  else
    mkdir -p "$(dirname "$root$dst")"
    [ -f "$root$dst" ] && [ ! -L "$root$dst" ] || { rm -f "$root$dst"; : > "$root$dst"; }
  fi
  mount --bind "$src" "$root$dst"
  if [ "$ro" = "1" ]; then
    mount -o remount,ro,bind "$root$dst" 2>/dev/null || true
  fi
}
i=0
while [ "$i" -lt "$rw_count" ]; do
  bind_one "$1" "$2" 0
  shift 2
  i=$((i + 1))
done
i=0
while [ "$i" -lt "$ro_count" ]; do
  bind_one "$1" "$2" 1
  shift 2
  i=$((i + 1))
done
export PATH="$target_path"
if [ -n "$userspec" ]; then
  if [ -n "$groups" ]; then
    exec chroot --userspec="$userspec" --groups="$groups" "$root" /bin/sh -c 'cd "$1" && shift && exec "$@"' hotbed "$dir" "$@"
  fi
  exec chroot --userspec="$userspec" "$root" /bin/sh -c 'cd "$1" && shift && exec "$@"' hotbed "$dir" "$@"
else
  exec chroot "$root" /bin/sh -c 'cd "$1" && shift && exec "$@"' hotbed "$dir" "$@"
fi
`

type resolvedUser struct {
	set    bool
	uid    int
	gid    int
	groups []string
}

func (u resolvedUser) userspec() string {
	if !u.set {
		return ""
	}
	return fmt.Sprintf("%d:%d", u.uid, u.gid)
}

func (u resolvedUser) groupsSpec() string {
	if !u.set || len(u.groups) == 0 {
		return ""
	}
	return strings.Join(u.groups, ",")
}

// resolveUser looks up a host user's numeric uid, gid, and supplementary gids.
// A package variable for tests.
var resolveUser = defaultResolveUser

func defaultResolveUser(name string) (resolvedUser, error) {
	if name == "" {
		return resolvedUser{}, nil
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		return resolvedUser{}, fmt.Errorf("look up user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return resolvedUser{}, fmt.Errorf("parse uid %q of %s: %w", u.Uid, name, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return resolvedUser{}, fmt.Errorf("parse gid %q of %s: %w", u.Gid, name, err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return resolvedUser{}, fmt.Errorf("groups of %s: %w", name, err)
	}
	return resolvedUser{set: true, uid: uid, gid: gid, groups: gids}, nil
}

// StartShell starts an interactive or piped shell inside a transient systemd
// scope unit (under s.Slice) chrooted into s.RootDir, with private /tmp, user,
// and environment. Using a systemd scope avoids passing file descriptors over
// D-Bus (which SELinux rejects under unconfined_service_t) while still keeping
// the entire process tree in its own cgroup for clean KillMode=control-group
// teardown.
func StartShell(ctx context.Context, s Spec, sio ShellIO) (ShellHandle, error) {
	if s.RootDir == "" {
		return nil, errors.New("root directory is required")
	}
	if err := s.prepareChroot(); err != nil {
		return nil, err
	}
	u, err := resolveUser(s.User)
	if err != nil {
		return nil, err
	}
	args, env, err := shellStartArgsFor(s, sio, u)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("systemd-run", args...)
	cmd.Env = env

	var ptmx, pts *os.File
	if sio.PTY {
		var slavePath string
		ptmx, pts, slavePath, err = pty.Open()
		if err != nil {
			return nil, fmt.Errorf("open pty: %w", err)
		}
		if u.set {
			if err := os.Chown(slavePath, u.uid, u.gid); err != nil {
				pts.Close()
				ptmx.Close()
				return nil, fmt.Errorf("chown pty %s: %w", slavePath, err)
			}
			if err := os.Chmod(slavePath, 0o600); err != nil {
				pts.Close()
				ptmx.Close()
				return nil, fmt.Errorf("chmod pty %s: %w", slavePath, err)
			}
		}
		if sio.Cols > 0 || sio.Rows > 0 {
			_ = pty.Resize(ptmx, sio.Cols, sio.Rows)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = sio.Stdin, sio.Stdout, sio.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		if pts != nil {
			pts.Close()
		}
		if ptmx != nil {
			ptmx.Close()
		}
		return nil, fmt.Errorf("start shell %s: %w", s.Name, err)
	}
	if pts != nil {
		pts.Close()
	}

	h := &scopeShell{
		name: s.Name,
		cmd:  cmd,
		ptmx: ptmx,
		done: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = exitErr.ExitCode()
			}
		} else if err != nil {
			code = 1
		}
		h.exitCode = code
		close(h.done)
	}()
	// If the caller's context cancels, stop the shell scope.
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = h.Close()
			case <-h.done:
			}
		}()
	}
	return h, nil
}

// shellStartArgs builds the systemd-run --scope arguments and process
// environment for StartShell.
func shellStartArgs(s Spec, sio ShellIO) ([]string, []string, error) {
	if s.RootDir == "" {
		return nil, nil, errors.New("root directory is required")
	}
	u, err := resolveUser(s.User)
	if err != nil {
		return nil, nil, err
	}
	return shellStartArgsFor(s, sio, u)
}

// shellStartArgsFor builds the systemd-run --scope arguments and process
// environment for StartShell with an already-resolved user.
//
// Scope units have no ExecContext, so systemd-run --scope rejects execution
// properties such as OOMScoreAdjust= and SELinuxContext= with "Unknown
// assignment". Do not pass SELinuxContext= here (board 7921).
func shellStartArgsFor(s Spec, sio ShellIO, u resolvedUser) ([]string, []string, error) {
	if s.RootDir == "" {
		return nil, nil, errors.New("root directory is required")
	}
	unshare, err := lookPath("unshare")
	if err != nil {
		return nil, nil, fmt.Errorf("find unshare: %w", err)
	}
	argv := s.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/bash"}
	}

	rwBinds := append(slices.Clone(s.Binds), s.tmpBinds()...)
	roBinds := s.ReadOnlyBinds

	args := []string{
		"--quiet",
		"--scope",
		"--unit=" + s.Name,
		"--slice=" + cmp.Or(s.Slice, DefaultSlice),
		"--description=" + description(argv),
		"--property=KillMode=control-group",
		"--property=MemoryMax=infinity",
		"--",
		unshare, "--mount", "--propagation", "slave", "--",
		"/bin/sh", "-eu", "-c", shellOOMScoreAdj + chrootScript, "hotbed-shell",
		s.RootDir, cmp.Or(s.Dir, "/"), u.userspec(), u.groupsSpec(),
		strconv.Itoa(len(rwBinds)), strconv.Itoa(len(roBinds)),
	}
	for _, b := range rwBinds {
		args = append(args, b.Source, path.Clean("/"+b.Target))
	}
	for _, b := range roBinds {
		args = append(args, b.Source, path.Clean("/"+b.Target))
	}
	args = append(args, argv...)

	term := sio.Term
	if term == "" {
		if sio.PTY {
			term = "xterm-256color"
		} else {
			term = "dumb"
		}
	}

	var env []string
	env = append(env, "TERM="+term, "SHELL=/bin/bash")
	if s.User != "" {
		env = append(env, "USER="+s.User, "LOGNAME="+s.User)
	}
	if s.Tmp != "" {
		env = append(env, "TMPDIR="+TmpPaths[0])
	}
	env = append(env, s.Env...)
	env = append(env, "PATH="+s.rootfsPath())
	return args, env, nil
}

type scopeShell struct {
	name      string
	cmd       *exec.Cmd
	ptmx      *os.File
	done      chan struct{}
	exitCode  int
	closeOnce sync.Once
}

func (h *scopeShell) PTY() *os.File { return h.ptmx }

func (h *scopeShell) Resize(cols, rows uint16) error {
	if h.ptmx == nil {
		return nil
	}
	return pty.Resize(h.ptmx, cols, rows)
}

func (h *scopeShell) Wait() int {
	<-h.done
	// Ensure any orphaned children in the scope's cgroup are stopped so the
	// PTY slave closes cleanly and buffered output finishes draining.
	h.stopScope()
	return h.exitCode
}

func (h *scopeShell) Close() error {
	h.closeOnce.Do(func() {
		if h.ptmx != nil {
			h.ptmx.Close()
		}
		h.stopScope()
		if h.cmd.Process != nil {
			_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return nil
}

func (h *scopeShell) stopScope() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = run(ctx, "systemctl", "stop", h.name+".scope")
	_, _ = run(ctx, "systemctl", "reset-failed", h.name+".scope")
}
