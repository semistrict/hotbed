package inspect

import (
	"bufio"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// ClampSampleDuration bounds a process sample duration in seconds to [1, 30] (default 3).
func ClampSampleDuration(sec int32) int {
	if sec <= 0 {
		return 3
	}
	if sec > 30 {
		return 30
	}
	return int(sec)
}

const minPerfFallbackDuration = 100 * time.Millisecond

// perfFallbackSleepArg formats the remaining sample window duration as a
// fractional seconds argument for `sleep` when retrying `perf stat` against a
// cgroup. Returns ("", false) when too little window remains to be useful.
func perfFallbackSleepArg(rem time.Duration) (string, bool) {
	if rem < minPerfFallbackDuration {
		return "", false
	}
	return strconv.FormatFloat(rem.Seconds(), 'f', 3, 64), true
}

// ---- Linux syscall tables (amd64 & arm64) ----------------------------------

// SyscallName resolves a Linux syscall number for arch ("amd64" or "arm64").
func SyscallName(arch string, nr int64) string {
	if nr < 0 {
		return "running"
	}
	var m map[int64]string
	switch arch {
	case "arm64", "aarch64":
		m = syscallsARM64
	default:
		m = syscallsAMD64
	}
	if name, ok := m[nr]; ok {
		return name
	}
	return fmt.Sprintf("syscall_%d", nr)
}

// SyscallTakesFDArg0 reports whether arg0 of the named Linux syscall is a file
// descriptor that can be correlated against /proc/<pid>/fd/<n>.
func SyscallTakesFDArg0(name string) bool {
	switch name {
	case "read", "write", "close", "fstat", "lseek", "pread64", "pwrite64",
		"readv", "writev", "preadv", "pwritev", "preadv2", "pwritev2",
		"ioctl", "fcntl", "flock", "fsync", "fdatasync", "ftruncate",
		"getdents", "getdents64", "fchdir", "fchmod", "fchown",
		"connect", "accept", "accept4", "sendto", "recvfrom", "sendmsg", "recvmsg",
		"shutdown", "bind", "listen", "getsockname", "getpeername",
		"setsockopt", "getsockopt", "sendfile", "splice", "tee",
		"epoll_wait", "epoll_pwait", "epoll_pwait2", "epoll_ctl",
		"io_uring_enter", "recvmmsg", "sendmmsg", "copy_file_range":
		return true
	}
	return false
}

var syscallsAMD64 = map[int64]string{
	0: "read", 1: "write", 2: "open", 3: "close", 4: "stat", 5: "fstat", 6: "lstat",
	7: "poll", 8: "lseek", 9: "mmap", 10: "mprotect", 11: "munmap", 12: "brk",
	13: "rt_sigaction", 14: "rt_sigprocmask", 15: "rt_sigreturn", 16: "ioctl",
	17: "pread64", 18: "pwrite64", 19: "readv", 20: "writev", 21: "access",
	22: "pipe", 23: "select", 24: "sched_yield", 25: "mremap", 26: "msync",
	28: "madvise", 32: "dup", 33: "dup2", 34: "pause", 35: "nanosleep",
	38: "setitimer", 39: "getpid", 40: "sendfile", 41: "socket", 42: "connect",
	43: "accept", 44: "sendto", 45: "recvfrom", 46: "sendmsg", 47: "recvmsg",
	48: "shutdown", 49: "bind", 50: "listen", 51: "getsockname", 52: "getpeername",
	53: "socketpair", 54: "setsockopt", 55: "getsockopt", 56: "clone", 57: "fork",
	58: "vfork", 59: "execve", 60: "exit", 61: "wait4", 62: "kill",
	72: "fcntl", 73: "flock", 74: "fsync", 75: "fdatasync", 77: "ftruncate",
	78: "getdents", 79: "getcwd", 80: "chdir", 81: "fchdir", 82: "rename",
	83: "mkdir", 84: "rmdir", 87: "unlink", 89: "readlink",
	128: "rt_sigtimedwait", 130: "rt_sigsuspend", 137: "statfs", 138: "fstatfs",
	162: "sync", 186: "gettid", 202: "futex", 217: "getdents64",
	228: "clock_gettime", 230: "clock_nanosleep", 231: "exit_group",
	232: "epoll_wait", 233: "epoll_ctl", 247: "waitid", 257: "openat",
	258: "mkdirat", 262: "newfstatat", 263: "unlinkat", 264: "renameat",
	267: "readlinkat", 270: "pselect6", 271: "ppoll", 275: "splice", 276: "tee",
	281: "epoll_pwait", 288: "accept4", 290: "eventfd2", 291: "epoll_create1",
	292: "dup3", 293: "pipe2", 295: "preadv", 296: "pwritev", 299: "recvmmsg",
	302: "prlimit64", 307: "sendmmsg", 316: "renameat2", 318: "getrandom",
	322: "execveat", 326: "copy_file_range", 327: "preadv2", 328: "pwritev2",
	332: "statx", 426: "io_uring_enter", 435: "clone3", 437: "openat2",
	441: "epoll_pwait2", 449: "futex_waitv",
}

var syscallsARM64 = map[int64]string{
	17: "getcwd", 19: "eventfd2", 20: "epoll_create1", 21: "epoll_ctl",
	22: "epoll_pwait", 23: "dup", 24: "dup3", 25: "fcntl", 29: "ioctl",
	32: "flock", 34: "mkdirat", 35: "unlinkat", 38: "renameat", 43: "statfs",
	44: "fstatfs", 46: "ftruncate", 49: "chdir", 50: "fchdir", 56: "openat",
	57: "close", 59: "pipe2", 61: "getdents64", 62: "lseek", 63: "read",
	64: "write", 65: "readv", 66: "writev", 67: "pread64", 68: "pwrite64",
	69: "preadv", 70: "pwritev", 71: "sendfile", 72: "pselect6", 73: "ppoll",
	76: "splice", 77: "tee", 78: "readlinkat", 79: "newfstatat", 80: "fstat",
	81: "sync", 82: "fsync", 83: "fdatasync", 93: "exit", 94: "exit_group",
	95: "waitid", 98: "futex", 101: "nanosleep", 113: "clock_gettime",
	115: "clock_nanosleep", 124: "sched_yield", 129: "kill", 133: "rt_sigsuspend",
	134: "rt_sigaction", 135: "rt_sigprocmask", 137: "rt_sigtimedwait",
	139: "rt_sigreturn", 172: "getpid", 178: "gettid", 198: "socket",
	199: "socketpair", 200: "bind", 201: "listen", 202: "accept", 203: "connect",
	204: "getsockname", 205: "getpeername", 206: "sendto", 207: "recvfrom",
	208: "setsockopt", 209: "getsockopt", 210: "shutdown", 211: "sendmsg",
	212: "recvmsg", 214: "brk", 215: "munmap", 216: "mremap", 220: "clone",
	221: "execve", 222: "mmap", 226: "mprotect", 227: "msync", 233: "madvise",
	242: "accept4", 243: "recvmmsg", 260: "wait4", 261: "prlimit64",
	269: "sendmmsg", 276: "renameat2", 278: "getrandom", 281: "execveat",
	285: "copy_file_range", 286: "preadv2", 287: "pwritev2", 291: "statx",
	426: "io_uring_enter", 435: "clone3", 437: "openat2", 441: "epoll_pwait2",
	449: "futex_waitv",
}

// ---- /proc & tool output parsers -------------------------------------------

// ParseProcSyscall parses /proc/<pid>/task/<tid>/syscall.
// Format when blocked: "<nr> <a0> <a1> <a2> <a3> <a4> <a5> <sp> <pc>"
// Format when running in userspace: "running"
func ParseProcSyscall(arch, content string) *pb.ThreadSyscall {
	s := strings.TrimSpace(content)
	if s == "" {
		return nil
	}
	if s == "running" {
		return &pb.ThreadSyscall{Number: -1, Name: "running"}
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	nr, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return nil
	}
	out := &pb.ThreadSyscall{
		Number: nr,
		Name:   SyscallName(arch, nr),
	}
	if len(fields) >= 9 {
		out.Args = append(out.Args, fields[1:7]...)
		out.Sp = fields[7]
		out.Pc = fields[8]
	} else if len(fields) > 1 {
		out.Args = append(out.Args, fields[1:]...)
	}
	return out
}

// ParseSchedStat parses /proc/<pid>/task/<tid>/schedstat: "<run_ns> <wait_ns> <timeslices>".
func ParseSchedStat(content string) (runNs, waitNs, slices int64) {
	fields := strings.Fields(content)
	if len(fields) >= 3 {
		runNs, _ = strconv.ParseInt(fields[0], 10, 64)
		waitNs, _ = strconv.ParseInt(fields[1], 10, 64)
		slices, _ = strconv.ParseInt(fields[2], 10, 64)
	}
	return runNs, waitNs, slices
}

// ParseFDInfoDetailed parses /proc/<pid>/fdinfo/<fd>, including pos, open mode,
// file inode, and any epoll watched target FDs (`tfd:`).
func ParseFDInfoDetailed(content string) (pos int64, mode string, inode uint64, epollTFDs []int32) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "tfd:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if tfd, err := strconv.Atoi(fields[1]); err == nil {
					epollTFDs = append(epollTFDs, int32(tfd))
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "pos":
			pos, _ = strconv.ParseInt(v, 10, 64)
		case "ino":
			inode, _ = strconv.ParseUint(v, 10, 64)
		case "flags":
			if flags, err := strconv.ParseUint(v, 8, 64); err == nil {
				switch flags & 3 {
				case 0:
					mode = "r"
				case 1:
					mode = "w"
				case 2:
					mode = "rw"
				}
			}
		}
	}
	sort.Slice(epollTFDs, func(i, j int) bool { return epollTFDs[i] < epollTFDs[j] })
	return pos, mode, inode, epollTFDs
}

// RawFileLock is one entry parsed from /proc/locks.
type RawFileLock struct {
	Kind    string
	Mode    string
	Access  string
	PID     int32
	Inode   uint64
	Range   string
	Blocked bool
}

// ParseProcLocks parses /proc/locks lines, including blocked ("->") lock waiters.
func ParseProcLocks(content string) []RawFileLock {
	var out []RawFileLock
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		idx := 1
		blocked := false
		if fields[idx] == "->" {
			blocked = true
			idx++
		}
		if len(fields) < idx+7 {
			continue
		}
		kind := fields[idx]
		mode := fields[idx+1]
		access := fields[idx+2]
		pid, _ := strconv.Atoi(fields[idx+3])
		// fields[idx+4] is maj:min:inode
		var inode uint64
		if parts := strings.Split(fields[idx+4], ":"); len(parts) == 3 {
			inode, _ = strconv.ParseUint(parts[2], 10, 64)
		}
		rng := fields[idx+5] + ".." + fields[idx+6]
		out = append(out, RawFileLock{
			Kind:    kind,
			Mode:    mode,
			Access:  access,
			PID:     int32(pid),
			Inode:   inode,
			Range:   rng,
			Blocked: blocked,
		})
	}
	return out
}

// ParseEuStack parses `eu-stack [-m] [-s] -p <pid>` output into leaf->root frames per TID.
func ParseEuStack(output string) map[int32][]*pb.StackFrame {
	out := map[int32][]*pb.StackFrame{}
	var curTID int32
	var lastFrame *pb.StackFrame
	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "TID ") && strings.HasSuffix(trimmed, ":") {
			if tid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(trimmed, "TID "), ":")); err == nil {
				curTID = int32(tid)
				lastFrame = nil
			}
			continue
		}
		if curTID == 0 {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// Examples:
			//   #0  0x00007a28c1f5e3a6 __syscall_cancel - libc.so.6
			//   #1  0x00007a28c1fdaa8f wait4
			//   #2  0x00005c5ab09c3a31
			//   #3  0x00007f... func+0x12 - /usr/lib/foo.so
			fields := strings.Fields(trimmed)
			if len(fields) < 2 {
				continue
			}
			addr := fields[1]
			sym := ""
			mod := ""
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			rest = strings.TrimSpace(strings.TrimPrefix(rest, addr))
			if rest != "" {
				if strings.HasPrefix(rest, "- ") {
					// Unresolved symbol with `-m` module path: "#3 0xaddr - /usr/bin/python3.14"
					mod = strings.TrimSpace(rest[2:])
				} else if left, right, ok := strings.Cut(rest, " - "); ok {
					sym = strings.TrimSpace(left)
					mod = strings.TrimSpace(right)
				} else {
					sym = rest
				}
			}
			// Strip trailing "+0x..." offset if present, keeping clean symbol name.
			if plus := strings.LastIndex(sym, "+0x"); plus > 0 {
				sym = sym[:plus]
			}
			if mod != "" && strings.Contains(mod, "/") {
				mod = filepath.Base(mod)
			}
			if sym == "" {
				if mod != "" {
					sym = fmt.Sprintf("%s@%s", mod, addr)
				} else {
					sym = addr
				}
			}
			fr := &pb.StackFrame{
				Symbol:  sym,
				Module:  mod,
				Address: addr,
			}
			out[curTID] = append(out[curTID], fr)
			lastFrame = fr
			continue
		}
		// Optional source line emitted under `-s` (indented file:line).
		if lastFrame != nil && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")) && strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "eu-stack:") {
			if lastFrame.Source == "" {
				lastFrame.Source = trimmed
			}
		}
	}
	return out
}

var gdbThreadRe = regexp.MustCompile(`^Thread\s+\d+\s+\(.*?LWP\s+(\d+)`)
var gdbFrameRe = regexp.MustCompile(`^#\d+\s+(?:(0x[0-9a-fA-F]+)\s+in\s+)?([^\s(]+)(?:\s*\([^)]*\))?(?:\s+from\s+(\S+))?(?:\s+at\s+(\S+))?`)

// ParseGDBBacktrace parses `gdb -batch -ex "thread apply all bt"` output into leaf->root frames per TID.
func ParseGDBBacktrace(output string) map[int32][]*pb.StackFrame {
	out := map[int32][]*pb.StackFrame{}
	var curTID int32
	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		if m := gdbThreadRe.FindStringSubmatch(trimmed); m != nil {
			if tid, err := strconv.Atoi(m[1]); err == nil {
				curTID = int32(tid)
			}
			continue
		}
		if curTID == 0 || !strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := gdbFrameRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		addr, sym, mod, src := m[1], m[2], m[3], m[4]
		if mod != "" {
			mod = filepath.Base(mod)
		}
		if sym == "" {
			sym = addr
		}
		out[curTID] = append(out[curTID], &pb.StackFrame{
			Symbol:  sym,
			Module:  mod,
			Address: addr,
			Source:  src,
		})
	}
	return out
}

func nonEmpty(a, b string) string {
	if a != "" && a != "0" {
		return a
	}
	return b
}
