//go:build linux && systemdtest

// Run as root on a systemd machine:
//
//	sudo HOTBED_TEST_USER=$USER HOTBED_TEST_DISK=/mnt/fxdax-scratch go test -tags systemdtest ./internal/unit/
//
// A privileged unit runs the test binary (as it runs hotbed) before its
// command, on the host, where the unit's binds (its /tmp) are made: build it
// with go test -c and run it from a path none shadows that SELinux lets
// systemd run (bin_t), such as /usr/local/bin.
package unit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"errors"
	osuser "os/user"
	"strconv"

	"github.com/semistrict/hotbed/internal/sysinfo"
)

func testUser(t *testing.T) string {
	u := os.Getenv("HOTBED_TEST_USER")
	if u == "" {
		t.Fatal("set HOTBED_TEST_USER to the user commands run as")
	}
	return u
}

// waitExit waits for the unit's command to exit and returns its state.
func waitExit(t *testing.T, name string) State {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, DefaultSlice, name); err != nil {
		t.Fatalf("wait for %s: %v", name, err)
	}
	s, err := Status(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exited {
		t.Fatalf("%s has no processes left but is %+v", name, s)
	}
	return s
}

func name(t *testing.T) string {
	n := fmt.Sprintf("hotbed-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { Stop(context.WithoutCancel(t.Context()), n) })
	return n
}

func TestExitCodeAndLog(t *testing.T) {
	root, roBinds := minimalRoot(t)
	dir := t.TempDir()
	n := name(t)
	log := filepath.Join(dir, "out.log")
	err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, User: testUser(t), Log: log, CPUWeight: 50, MemoryLow: 64 << 20, MemoryHigh: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := waitExit(t, n)
	if s.ExitCode != 3 || s.Signal != 0 {
		t.Errorf("state %+v, want exit 3", s)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "out\nerr\n" {
		t.Errorf("log %q, want both streams", b)
	}
}

// A command's /tmp and /var/tmp are its Tmp: what it writes there lands
// in Tmp, and it sees nothing of the host's.
func TestTmpIsTheCommandsOwn(t *testing.T) {
	root, roBinds := minimalRoot(t)
	tmp := rootTmp(t, root)
	host, err := os.CreateTemp("/tmp", "hotbed-host-*")
	if err != nil {
		t.Fatal(err)
	}
	host.Close()
	t.Cleanup(func() { os.Remove(host.Name()) })
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	script := `echo "$TMPDIR"; test -e ` + host.Name() + ` && echo host; echo mine > /tmp/a; echo var > /var/tmp/b; ls /tmp`
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sh", "-c", script}, User: testUser(t), Tmp: tmp, Log: log}); err != nil {
		t.Fatal(err)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Fatalf("state %+v", s)
	}
	if b, _ := os.ReadFile(log); string(b) != "/tmp\na\nb\n" {
		t.Errorf("log %q, want TMPDIR=/tmp and only its own files in /tmp", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tmp, "a")); string(b) != "mine\n" {
		t.Errorf("%s/a = %q, want what it wrote to /tmp/a", tmp, b)
	}
	if _, err := os.Stat(filepath.Join("/tmp", "a")); err == nil {
		t.Errorf("/tmp/a is on the host")
	}
}

func TestFreezeThawStop(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sleep", "300"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	s, err := Status(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running || s.PID == 0 || !strings.HasSuffix(s.Cgroup, "/"+n+".service") {
		t.Fatalf("after start: %+v", s)
	}
	if err := Freeze(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); !s.Frozen {
		t.Errorf("after freeze: %+v", s)
	}
	if err := Reclaim(s.Cgroup, 1<<20); err != nil {
		t.Errorf("reclaim: %v", err)
	}
	// Reclaimed, its memory is in swap, but still its own.
	if m, err := Memory(s.Cgroup); err != nil || m.Held() == 0 {
		t.Errorf("memory: %+v, %v; want what it holds, in RAM or swap", m, err)
	}
	if err := Thaw(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); s.Frozen || !s.Running {
		t.Errorf("after thaw: %+v", s)
	}
	if err := Stop(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if s, _ := Status(t.Context(), n); s.Loaded {
		t.Errorf("after stop: %+v", s)
	}
}

func TestWaitReturnsWhenTheUnitExits(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sleep", "1"}, User: testUser(t), Log: filepath.Join(t.TempDir(), "out.log")}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := Wait(short, DefaultSlice, n); err != context.DeadlineExceeded {
		t.Fatalf("wait on a running unit = %v, want the deadline", err)
	}
	start := time.Now()
	waitExit(t, n)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("wait returned %v after a 1s command that had run for 0.2s", d)
	}
	if err := Wait(t.Context(), DefaultSlice, "hotbed-test-no-such-unit"); err != nil {
		t.Errorf("wait on a unit that is not there = %v, want nil", err)
	}
}

// The command must run inside its unit's cgroup, where its limits, freezing
// and reclaim apply; a PAM login session would move it to user.slice.
func TestCommandRunsInItsUnitsCgroup(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sh", "-c", `cat /proc/self/cgroup; while [ ! -e /work/done ]; do sleep 0.05; done`}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	cg, err := exec.Command("systemctl", "show", "--property=ControlGroup", "--value", n+".service").Output()
	if err != nil {
		t.Fatal(err)
	}
	wantHost := fmt.Sprintf("%s/%s.service", SliceDir(DefaultSlice), n)
	if got := strings.TrimSpace(string(cg)); got != wantHost {
		t.Errorf("host ControlGroup = %q, want %q", got, wantHost)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "done"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0::" + wantHost + "\n"; string(b) != want {
		t.Errorf("command saw %q, want %q", b, want)
	}
}

// A command's $variables are its shell's, not systemd's: systemd would
// substitute them in the command line before the shell runs.
func TestShellVariablesReachTheShell(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"sh", "-c", `x=shell; echo "[$x] [${x}]"`}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[shell] [shell]\n" {
		t.Errorf("command printed %q", b)
	}
}

// A worker may put its commands in a slice under another, such as
// user.slice, to share that slice's memory limit; Wait finds them there.
func TestNestedSlice(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	const slice = "user-hotbedtest.slice"
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Slice: slice, Argv: []string{"sh", "-c", "cat /proc/self/cgroup; while [ ! -e /work/done ]; do sleep 0.05; done; sleep 0.2"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	cg, err := exec.Command("systemctl", "show", "--property=ControlGroup", "--value", n+".service").Output()
	if err != nil {
		t.Fatal(err)
	}
	wantHost := "/user.slice/user-hotbedtest.slice/" + n + ".service"
	if got := strings.TrimSpace(string(cg)); got != wantHost {
		t.Errorf("host ControlGroup = %q, want %q", got, wantHost)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "done"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, slice, n); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0::" + wantHost + "\n"; string(b) != want {
		t.Errorf("command saw %q, want %q", b, want)
	}
}

// systemd rewrites a transient unit from its file on daemon-reload: a unit
// running a script of many lines comes through it whole.
func TestUnitOutlivesADaemonReload(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Argv: []string{"bash", "-c", "sleep 0.5\necho after"}, User: testUser(t), Log: log}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v: %s", err, out)
	}
	desc, err := exec.Command("systemctl", "show", "-P", "Description", n+".service").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(desc)); got != "hotbed bash -c sleep 0.5 echo after" {
		t.Errorf("description after the reload = %q", got)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Errorf("state %+v, want exit 0", s)
	}
	if b, _ := os.ReadFile(log); string(b) != "after\n" {
		t.Errorf("log %q", b)
	}
}

// A measurement's unit never swaps: memory.swap.max 0.
func TestNoSwapUnit(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, TraceFS: true, Argv: []string{"sh", "-c", `cat "/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)/memory.swap.max"`}, User: testUser(t), Log: log, NoSwap: true}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	if b, _ := os.ReadFile(log); string(b) != "0\n" {
		t.Errorf("a NoSwap unit's memory.swap.max = %q, want 0", b)
	}
}

// StartSlice starts a slice that has no unit file, and SetProperties sets,
// then resets, its properties.
func TestStartSliceAndSetProperties(t *testing.T) {
	const slice = "hotbedtest-props.slice"
	t.Cleanup(func() {
		// Stopped, and what set-property left of it removed: the host as it was.
		for _, args := range [][]string{{"stop", slice, "hotbedtest.slice"}, {"revert", slice}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				t.Errorf("systemctl %v: %v: %s", args, err, out)
			}
		}
	})
	if err := StartSlice(t.Context(), slice); err != nil {
		t.Fatal(err)
	}
	dir := "/sys/fs/cgroup" + SliceDir(slice)
	disk := os.Getenv("HOTBED_TEST_DISK")
	if disk == "" {
		t.Fatal("set HOTBED_TEST_DISK to a path on a block device, such as a scratch disk's mount")
	}
	if err := SetProperties(t.Context(), slice, "AllowedCPUs=0", "IOWriteBandwidthMax="+disk+" 32M"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dir + "/cpuset.cpus"); string(b) != "0\n" {
		t.Errorf("cpuset.cpus = %q, want 0", b)
	}
	// systemd's M is 1000², a disk's.
	if b, _ := os.ReadFile(dir + "/io.max"); !strings.Contains(string(b), " wbps=32000000 ") {
		t.Errorf("io.max = %q, want 32 MB/s of writes", b)
	}
	if err := SetProperties(t.Context(), slice, "AllowedCPUs=", "IOWriteBandwidthMax="); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dir + "/cpuset.cpus"); strings.TrimSpace(string(b)) != "" {
		t.Errorf("cpuset.cpus after the reset = %q, want none", b)
	}
	if b, _ := os.ReadFile(dir + "/io.max"); strings.Contains(string(b), "wbps=32000000") {
		t.Errorf("io.max after the reset = %q, want no cap", b)
	}
}

// A slice in a user's slice stays started when its last unit ends, its
// cgroup with it, though systemd's user-.slice.d says StopWhenUnneeded=yes
// for every user-*.slice: a measurement's partition, written there, would go
// as its unit ended (board 6842).
func TestStartSliceKeepsAUserSlice(t *testing.T) {
	const slice = "user-hotbedtest-keep.slice"
	t.Cleanup(func() {
		for _, args := range [][]string{{"stop", slice, "user-hotbedtest.slice"}, {"revert", slice}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				t.Errorf("systemctl %v: %v: %s", args, err, out)
			}
		}
	})
	if err := StartSlice(t.Context(), slice); err != nil {
		t.Fatal(err)
	}
	if got, err := Show(t.Context(), slice, "StopWhenUnneeded"); err != nil || strings.Join(got["StopWhenUnneeded"], ",") != "no" {
		t.Fatalf("StopWhenUnneeded = %q, %v; want no", got["StopWhenUnneeded"], err)
	}
	cgroup := func() uint64 {
		st, err := os.Stat("/sys/fs/cgroup" + SliceDir(slice))
		if err != nil {
			t.Fatal(err)
		}
		return st.Sys().(*syscall.Stat_t).Ino
	}
	before := cgroup()
	root, roBinds := minimalRoot(t)
	n := name(t)
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, Slice: slice, User: testUser(t), Log: filepath.Join(t.TempDir(), "out.log"), Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, slice, n); err != nil {
		t.Fatalf("wait for %s: %v", n, err)
	}
	if err := Stop(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if got, err := Show(t.Context(), slice, "ActiveState"); err != nil || strings.Join(got["ActiveState"], ",") != "active" {
		t.Errorf("after its last unit ended the slice is %q, %v; want active", got["ActiveState"], err)
	}
	if after := cgroup(); after != before {
		t.Errorf("after its last unit ended the slice's cgroup is a new one (inode %d, was %d)", after, before)
	}
}

// A work slice set to CPUWeight=idle and IOWeight=1 puts the slice in cgroup
// idle scheduling while still letting child units carry their own CPUWeight=,
// Nice=19, and IOSchedulingClass=idle.
func TestLowPriorityWorkSlice(t *testing.T) {
	const slice = "hotbedtest-lowprio.slice"
	t.Cleanup(func() {
		for _, args := range [][]string{{"stop", slice, "hotbedtest.slice"}, {"revert", slice}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				t.Errorf("systemctl %v: %v: %s", args, err, out)
			}
		}
	})
	if err := StartSlice(t.Context(), slice); err != nil {
		t.Fatal(err)
	}
	if err := SetProperties(t.Context(), slice, "CPUWeight=idle", "IOWeight=1"); err != nil {
		t.Fatal(err)
	}
	dir := "/sys/fs/cgroup" + SliceDir(slice)
	if b, _ := os.ReadFile(dir + "/cpu.idle"); string(b) != "1\n" {
		t.Errorf("cpu.idle = %q, want 1", b)
	}
	if b, _ := os.ReadFile(dir + "/io.weight"); strings.TrimSpace(string(b)) != "default 1" {
		t.Errorf("io.weight = %q, want default 1", b)
	}
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{
		Name: n, RootDir: root, ReadOnlyBinds: roBinds, Slice: slice, User: testUser(t), Log: log,
		CPUWeight:  100,
		Properties: []string{"Nice=19", "IOSchedulingClass=idle"},
		Argv:       []string{"sh", "-c", `IFS= read -r line </proc/self/stat; rest=${line##*) }; set -- $rest; echo "${17}"`},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Wait(ctx, slice, n); err != nil {
		t.Fatalf("wait for %s: %v", n, err)
	}
	s, err := Status(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exited || s.ExitCode != 0 {
		t.Fatalf("state %+v, want exit 0", s)
	}
	if b, _ := os.ReadFile(log); strings.TrimSpace(string(b)) != "19" {
		t.Errorf("nice = %q, want 19", b)
	}
}

// RootDir runs a command chrooted inside a dedicated rootfs with MountAPIVFS,
// a bound /tmp, and CAP_PERFMON ambient capability, isolated from host /.
// minimalRoot is a rootfs of the host's /usr, /bin and libraries, read-only,
// and the host's users.
func minimalRoot(t *testing.T) (string, []Bind) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"etc", "proc", "sys", "dev", "run", "tmp", "var/tmp", "work"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "chroot-marker"), []byte("inside-chroot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Copy host /etc/passwd and /etc/group so systemd resolves User= inside RootDirectory.
	for _, f := range []string{"passwd", "group"} {
		b, err := os.ReadFile("/etc/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var roBinds []Bind
	for _, sysDir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		if fi, err := os.Stat(sysDir); err == nil && fi.IsDir() {
			roBinds = append(roBinds, Bind{Source: sysDir, Target: sysDir})
		}
	}
	return root, roBinds
}

// Root inside a chroot sets its own unit's limits, as `sudo tee memory.max`
// does (board 7418).
func TestChrootRootSetsItsOwnLimits(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{
		Name: n, RootDir: root, Dir: "/work", Tmp: rootTmp(t, root), ReadOnlyBinds: roBinds, TraceFS: true, Log: log,
		Argv: []string{"sh", "-c", `c=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup); echo 2147483648 > $c/memory.max && cat $c/memory.max`},
	}); err != nil {
		t.Fatal(err)
	}
	s := waitExit(t, n)
	b, _ := os.ReadFile(log)
	if s.ExitCode != 0 || string(b) != "2147483648\n" {
		t.Errorf("root setting its memory.max in RootDir: exit %d, output %q; want 2147483648", s.ExitCode, b)
	}
}

func TestChrootIsolationAndCapabilities(t *testing.T) {
	root, roBinds := minimalRoot(t)
	user := testUser(t)
	if err := chownToUser(filepath.Join(root, "work"), user); err != nil {
		t.Fatal(err)
	}
	tmpHost := rootTmp(t, root)

	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	err := Start(t.Context(), Spec{
		Name:          n,
		RootDir:       root,
		Dir:           "/work",
		User:          user,
		Tmp:           tmpHost,
		ReadOnlyBinds: roBinds,
		Capabilities:  []string{"CAP_PERFMON"},
		TraceFS:       true,
		Log:           log,
		Argv: []string{"sh", "-c",
			`cat /etc/chroot-marker; echo "pwd=$(pwd)"; echo ok > "$TMPDIR/probe"; grep ^CapAmb: /proc/self/status; grep ^NoNewPrivs: /proc/self/status; cat /proc/self/uid_map`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		b, _ := os.ReadFile(log)
		t.Fatalf("state %+v, log:\n%s", s, b)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "inside-chroot\n") || !strings.Contains(out, "pwd=/work\n") {
		t.Errorf("unexpected chroot output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(tmpHost, "probe")); err != nil {
		t.Errorf("$TMPDIR/probe did not land in the command's tmp: %v", err)
	}
	// CAP_PERFMON is bit 38 (0x4000000000); CapAmb must be non-zero.
	if strings.Contains(out, "CapAmb:\t0000000000000000") {
		t.Errorf("expected non-zero CapAmb with CAP_PERFMON, got:\n%s", out)
	}
	// NoNewPrivs must be 0 so passwordless sudo to real root works.
	if !strings.Contains(out, "NoNewPrivs:\t0\n") {
		t.Errorf("expected NoNewPrivs: 0 so sudo works, got:\n%s", out)
	}
	// Identity uid_map confirms real host root/UIDs with no user namespace.
	if !strings.Contains(strings.Join(strings.Fields(out), " "), "0 0 4294967295") {
		t.Errorf("expected identity uid_map (no user namespace), got:\n%s", out)
	}
}

// A chrooted shell's terminal has a name inside /dev/pts and is owned by the user.
func TestChrootShellsTerminalHasANameInside(t *testing.T) {
	root, roBinds := minimalRoot(t)
	user := testUser(t)
	if err := chownToUser(filepath.Join(root, "work"), user); err != nil {
		t.Fatal(err)
	}
	h, err := StartShell(t.Context(), Spec{
		Name: name(t), RootDir: root, Dir: "/work", User: user, ReadOnlyBinds: roBinds,
		Argv: []string{"sh", "-c", `echo "$(tty)"; stat -c %U "$(tty)"`},
	}, ShellIO{PTY: true, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	out, _ := io.ReadAll(h.PTY()) // to EIO: the shell has closed its terminal
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n")), "\n")
	if code := h.Wait(); code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "/dev/pts/") || lines[1] != user {
		t.Errorf("exit %d, terminal showed %q; want /dev/pts/* and %q", code, out, user)
	}
}

// When the kernel OOM killer kills a child process inside a unit's cgroup,
// systemd marks Result=oom-kill and (under default OOMPolicy=stop) stops the
// main process with SIGTERM (15). Status must report OOMKilled=true rather
// than leaving callers thinking it was an unexplained SIGTERM (board 7232).
func TestOOMKillReported(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	err := Start(t.Context(), Spec{
		Name:          n,
		RootDir:       root,
		ReadOnlyBinds: roBinds,
		User:          testUser(t),
		Log:           log,
		NoSwap:     true,
		Properties: []string{"MemoryMax=64M"},
		// Spawn a child that faults 192 MiB of anonymous pages while the parent
		// shell waits on it, matching how cargo/bash hosts a child rustc/ld
		// that gets OOM-killed. Keep MemoryMax well above python3's startup
		// text/shared-lib footprint (so cgroup reclaim cannot livelock thrashing
		// clean executable pages under Ubuntu 26.04), and keep "; true" so dash
		// forks python3 instead of exec'ing it in place.
		Argv: []string{"sh", "-c", `python3 -c 'b = b"x" * (192*1024*1024); import time; time.sleep(10)'; true`},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := waitExit(t, n)
	if !s.OOMKilled {
		t.Fatalf("after cgroup OOM kill: state = %+v, want OOMKilled=true (Result=%q, Signal=%d, ExitCode=%d)", s, s.Result, s.Signal, s.ExitCode)
	}
}

// A command's processes, children too, are the OOM killer's first pick: a
// slice in user.slice shares its limit with the user's desktop, which the
// kernel otherwise killed before a build's linkers.
func TestCommandsAreTheOOMKillersFirstPick(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{Name: n, RootDir: root, ReadOnlyBinds: roBinds, User: testUser(t), Log: log, Argv: []string{"sh", "-c", "cat /proc/self/oom_score_adj"}}); err != nil {
		t.Fatal(err)
	}
	waitExit(t, n)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(b)), fmt.Sprint(OOMScoreAdjust); got != want {
		t.Errorf("a command's child has oom_score_adj %q, want %s", got, want)
	}
}

// chownToUser gives path to user and its login group. (Not chown(1):
// Ubuntu's uutils chown leaves the group of "user:" as it was.)
func chownToUser(path, name string) error {
	u, err := osuser.Lookup(name)
	if err != nil {
		return err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

// rootTmp is a command's tmp on its rootfs, as the worker makes it.
func rootTmp(t *testing.T, root string) string {
	t.Helper()
	tmp := filepath.Join(root, ".tmp", "c")
	if err := os.MkdirAll(tmp, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o1777); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// Commands that together want more memory than their slice's backstop,
// none of them alone, are bounded together: they wait, held back inside
// the slice, and the host answers all along. On ubu1 a base build and session
// builds, each in its own cgroup with nothing bounding their sum, took the
// host down for hours (board 9128). The slice here is bounded as a 4 GiB
// host's work slice would be (3.5 GiB), whatever this host has, so the test
// oversubscribes it without putting the host itself at risk; its commands
// cannot swap, so they cannot spill past it.
func TestBackstopBoundsCommandsTogether(t *testing.T) {
	const slice = "hotbedbackstoptest.slice"
	b := BackstopFor(4<<30, Swap{})
	if err := StartSlice(t.Context(), slice); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_ = exec.CommandContext(ctx, "systemctl", "stop", slice).Run()
		_ = exec.CommandContext(ctx, "systemctl", "revert", slice).Run()
	})
	if err := SetProperties(t.Context(), slice, b.Properties()...); err != nil {
		t.Fatal(err)
	}

	root, roBinds := minimalRoot(t)
	// Three commands of 1.5 GiB each: 4.5 GiB, past the slice's 3.5.
	var names []string
	for i := range 3 {
		n := fmt.Sprintf("%s-%d", name(t), i)
		names = append(names, n)
		err := Start(t.Context(), Spec{
			Name: n, Slice: slice, RootDir: root, ReadOnlyBinds: roBinds, User: testUser(t), NoSwap: true,
			Log:  filepath.Join(t.TempDir(), "out.log"),
			Argv: []string{"python3", "-c", "import time; b = b'x' * (1536 << 20); time.sleep(20)"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = Stop(context.WithoutCancel(t.Context()), n) })
	}

	// The host answers within a second all along, and the slice never
	// holds more than its bound.
	sliceDir := "/sys/fs/cgroup" + SliceDir(slice)
	var slowest time.Duration
	var peak int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		if err := exec.CommandContext(t.Context(), "true").Run(); err != nil {
			t.Fatal(err)
		}
		slowest = max(slowest, time.Since(start))
		if v, err := os.ReadFile(sliceDir + "/memory.current"); err == nil {
			n, _ := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
			peak = max(peak, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if slowest > time.Second {
		t.Errorf("the host took %s to run true while the slice was oversubscribed, want under 1s", slowest)
	}
	if peak > b.Max {
		t.Errorf("the slice held %d bytes, past its bound %d", peak, b.Max)
	}

	// None got all it asked for: held back past High, they wait there
	// (and the worker parks one, for the pressure), short of Max, where
	// one would be killed.
	for _, n := range names {
		st, err := Status(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running || st.Memory >= 1536<<20 {
			t.Errorf("%s: %+v; want it running, held under the 1.5 GiB it asked for", n, st)
		}
	}
	if peak <= b.High {
		t.Errorf("the slice peaked at %d bytes, under its High %d: the commands were not oversubscribed", peak, b.High)
	}
	t.Logf("slice peak %.2f GiB (high %.2f, max %.2f); slowest probe %s", float64(peak)/(1<<30), float64(b.High)/(1<<30), float64(b.Max)/(1<<30), slowest)
}

// On a zram host, swap lives in host RAM outside memory.current (board 9361).
// A zram-aware backstop applies MemorySwapMax alongside MemoryHigh/MemoryMax
// so swap-enabled commands cannot spill past the swap cap into host RAM, and
// applying an unbounded backstop explicitly clears any stale finite runtime
// MemorySwapMax back to "max" (board 9588).
func TestBackstopBoundsZramSwap(t *testing.T) {
	readInt64 := func(t *testing.T, path string) int64 {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			t.Fatalf("parse %s=%q: %v", path, strings.TrimSpace(string(raw)), err)
		}
		return v
	}
	readEvent := func(t *testing.T, path, key string) int64 {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		v, err := statField(string(raw), key)
		if err != nil {
			t.Fatalf("parse %s key %q from %q: %v", path, key, strings.TrimSpace(string(raw)), err)
		}
		return v
	}

	// Property application and finite -> unbounded ("max") transition run on
	// every systemd host regardless of whether zram is currently active.
	t.Run("properties", func(t *testing.T) {
		const slice = "hotbedzrambackstopprops.slice"
		finite := BackstopFor(4<<30, Swap{Zram: 8 << 30})
		if finite.SwapMax <= 0 {
			t.Fatalf("BackstopFor with zram produced non-positive SwapMax: %+v", finite)
		}
		if err := StartSlice(t.Context(), slice); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			_ = exec.CommandContext(ctx, "systemctl", "stop", slice).Run()
			_ = exec.CommandContext(ctx, "systemctl", "revert", slice).Run()
		})
		if err := SetProperties(t.Context(), slice, finite.Properties()...); err != nil {
			t.Fatal(err)
		}

		sliceDir := "/sys/fs/cgroup" + SliceDir(slice)
		for _, want := range []struct {
			file string
			val  int64
		}{
			{"memory.high", finite.High},
			{"memory.max", finite.Max},
			{"memory.swap.max", finite.SwapMax},
		} {
			if got := readInt64(t, filepath.Join(sliceDir, want.file)); got != want.val {
				t.Fatalf("%s = %d, want %d", want.file, got, want.val)
			}
		}

		// Applying an unbounded backstop on the same slice must explicitly reset
		// memory.swap.max back to "max" instead of leaving the stale finite cap.
		unbounded := BackstopFor(4<<30, Swap{Disk: 8 << 30})
		if unbounded.SwapMax != SwapMaxUnbounded {
			t.Fatalf("BackstopFor with disk-only swap produced SwapMax=%d, want %d", unbounded.SwapMax, SwapMaxUnbounded)
		}
		if err := SetProperties(t.Context(), slice, unbounded.Properties()...); err != nil {
			t.Fatal(err)
		}
		if got := readInt64(t, filepath.Join(sliceDir, "memory.high")); got != unbounded.High {
			t.Fatalf("memory.high after unbounded apply = %d, want %d", got, unbounded.High)
		}
		if got := readInt64(t, filepath.Join(sliceDir, "memory.max")); got != unbounded.Max {
			t.Fatalf("memory.max after unbounded apply = %d, want %d", got, unbounded.Max)
		}
		raw, err := os.ReadFile(filepath.Join(sliceDir, "memory.swap.max"))
		if err != nil {
			t.Fatalf("read memory.swap.max after unbounded apply: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != "max" {
			t.Fatalf("memory.swap.max after unbounded apply = %q, want %q", got, "max")
		}
	})

	// Live swap-enforcement test: requires real active zram swap on the host so
	// we exercise actual swapping and cgroup swap limit enforcement rather than
	// silently passing on a host without swap.
	t.Run("enforce", func(t *testing.T) {
		swaps, err := sysinfo.Swaps()
		if err != nil {
			t.Fatalf("sysinfo.Swaps: %v", err)
		}
		if swaps.ZramBytes == 0 {
			t.Skipf("host has no active zram swap (disk=%d bytes); skipping live zram enforcement test", swaps.DiskBytes)
		}

		const slice = "hotbedzrambackstoptest.slice"
		b := BackstopFor(4<<30, Swap{Zram: swaps.ZramBytes})
		if b.SwapMax <= 0 {
			t.Fatalf("BackstopFor with active zram (%d bytes) produced non-positive SwapMax: %+v", swaps.ZramBytes, b)
		}
		if err := StartSlice(t.Context(), slice); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			_ = exec.CommandContext(ctx, "systemctl", "stop", slice).Run()
			_ = exec.CommandContext(ctx, "systemctl", "revert", slice).Run()
		})
		if err := SetProperties(t.Context(), slice, b.Properties()...); err != nil {
			t.Fatal(err)
		}

		sliceDir := "/sys/fs/cgroup" + SliceDir(slice)
		swapEventsPath := filepath.Join(sliceDir, "memory.swap.events")
		swapCurrentPath := filepath.Join(sliceDir, "memory.swap.current")
		swapPeakPath := filepath.Join(sliceDir, "memory.swap.peak")
		memCurrentPath := filepath.Join(sliceDir, "memory.current")

		eventsMaxBefore := readEvent(t, swapEventsPath, "max")

		// Verify swap-enabled commands allocating past Max + SwapMax stay bounded
		// in both RAM and swap while actually swapping and hitting memory.swap.max.
		perProcMiB := (b.Max+b.SwapMax)/(3*(1<<20)) + 256
		root, roBinds := minimalRoot(t)
		for i := range 3 {
			n := fmt.Sprintf("%s-zram-%d", name(t), i)
			err := Start(t.Context(), Spec{
				Name: n, Slice: slice, RootDir: root, ReadOnlyBinds: roBinds, User: testUser(t),
				Log: filepath.Join(t.TempDir(), "out.log"),
				// Repeat a 1 MiB random chunk so every 4 KiB page is incompressible
				// while allocating quickly without burning CPU on urandom.
				Argv: []string{"python3", "-c", fmt.Sprintf("import os, time; c = os.urandom(1 << 20); b = bytearray(c) * %d; time.sleep(20)", perProcMiB)},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = Stop(context.WithoutCancel(t.Context()), n) })
		}

		var slowest time.Duration
		var peakMem, peakSwap, peakSwapCur, eventsMaxDuring int64
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			start := time.Now()
			if err := exec.CommandContext(t.Context(), "true").Run(); err != nil {
				t.Fatal(err)
			}
			slowest = max(slowest, time.Since(start))
			peakMem = max(peakMem, readInt64(t, memCurrentPath))
			curSwap := readInt64(t, swapCurrentPath)
			peakSwapCur = max(peakSwapCur, curSwap)
			peakSwap = max(peakSwap, curSwap, readInt64(t, swapPeakPath))
			eventsMaxDuring = max(eventsMaxDuring, readEvent(t, swapEventsPath, "max"))
			time.Sleep(100 * time.Millisecond)
		}
		if slowest > time.Second {
			t.Errorf("host took %s to run true while zram slice was oversubscribed, want under 1s", slowest)
		}
		if peakMem > b.Max {
			t.Errorf("slice memory.current peaked at %d bytes, past Max %d", peakMem, b.Max)
		}
		if peakSwap > b.SwapMax {
			t.Errorf("slice swap peaked at %d bytes (current peak %d), past SwapMax %d", peakSwap, peakSwapCur, b.SwapMax)
		}
		if peakMem <= b.High {
			t.Errorf("slice memory.current peaked at %d bytes, under High %d: commands were not oversubscribed", peakMem, b.High)
		}
		if peakSwap <= 0 {
			t.Errorf("slice peak swap = %d bytes, want > 0 (no swap activity occurred)", peakSwap)
		}
		if eventsMaxDuring <= eventsMaxBefore {
			t.Errorf("slice memory.swap.events max did not rise while workload was active (before=%d, during=%d)", eventsMaxBefore, eventsMaxDuring)
		}
		t.Logf("zram slice peak mem %.2f GiB (high %.2f, max %.2f), peak swap %.2f GiB (swapMax %.2f), swap.events max %d->%d; slowest probe %s",
			float64(peakMem)/(1<<30), float64(b.High)/(1<<30), float64(b.Max)/(1<<30),
			float64(peakSwap)/(1<<30), float64(b.SwapMax)/(1<<30), eventsMaxBefore, eventsMaxDuring, slowest)
	})
}

// Exec runs git chrooted inside the workspace rootfs as its user.
func TestExecRunsChrootedInRootfs(t *testing.T) {
	root, roBinds := minimalRoot(t)
	user := testUser(t)
	if err := chownToUser(filepath.Join(root, "work"), user); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "work", "r")
	planted := `id -u > /work/who; cat /etc/chroot-marker >> /work/who`
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "core.fsmonitor", planted}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := exec.Command("chown", "-R", "--reference="+filepath.Join(root, "work"), repo).Run(); err != nil {
		t.Fatal(err)
	}
	n := ExecPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	cmd, err := Exec(t.Context(), n, "", ExecSpec{Root: root, Dir: "/work/r", User: user, ReadOnlyBinds: roBinds, Argv: []string{"git", "status", "--porcelain"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "HOME=/work", "PATH=/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git status in the rootfs: %v %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(root, "work", "who"))
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	u, err := osuser.Lookup(user)
	if err != nil {
		t.Fatal(err)
	}
	if want := u.Uid + "\ninside-chroot\n"; string(b) != want {
		t.Errorf("the hook saw %q, want %q: the rootfs's user inside the chroot", b, want)
	}
	if out, _ := exec.Command("systemctl", "is-active", n+".scope").Output(); strings.TrimSpace(string(out)) == "active" {
		t.Errorf("git's scope %s is still active", n)
	}
}

// An Exec scope left by a worker that is gone (its git still running) is
// stopped as the next starts.
func TestStopLeftExecs(t *testing.T) {
	root, roBinds := minimalRoot(t)
	n := ExecPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	cmd, err := Exec(context.WithoutCancel(t.Context()), n, "", ExecSpec{Root: root, Dir: "/", ReadOnlyBinds: roBinds, Argv: []string{"sleep", "1000.75"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin")
	var said bytes.Buffer
	cmd.Stdout, cmd.Stderr = &said, &said
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for exec.Command("pgrep", "-f", "^sleep 1000.75").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("the Exec never started: %s", said.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := StopLeftExecs(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the left Exec is still running")
	}
	if out, err := exec.Command("pgrep", "-af", "^sleep 1000.75").Output(); err == nil {
		t.Errorf("its sleep outlived it: %s", out)
	}
	// With none left, there is nothing to stop.
	if err := StopLeftExecs(t.Context()); err != nil {
		t.Errorf("with no Exec left: %v", err)
	}
}

// On SELinux-enabled hosts, command units run in unconfined_t so tools like
// rsync do not transition into confined domains (board 7921).
func TestSELinuxContextUnconfined(t *testing.T) {
	if !hasSELinux() {
		t.Skip("SELinux not active on this host")
	}
	root, roBinds := minimalRoot(t)
	n := name(t)
	log := filepath.Join(t.TempDir(), "out.log")
	if err := Start(t.Context(), Spec{
		Name: n, RootDir: root, ReadOnlyBinds: roBinds, User: testUser(t), Log: log,
		Argv: []string{"sh", "-c", "cat /proc/self/attr/current"},
	}); err != nil {
		t.Fatal(err)
	}
	if s := waitExit(t, n); s.ExitCode != 0 {
		t.Fatalf("state %+v", s)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(b), "\x00\n")
	if got != unconfinedSELinuxContext {
		t.Errorf("command SELinux context = %q, want %q", got, unconfinedSELinuxContext)
	}

	// Verify rsync runs without transitioning into a confined domain (board 7921).
	t.Run("rsync", func(t *testing.T) {
		if _, err := exec.LookPath("rsync"); err != nil {
			t.Skipf("rsync not installed: %v", err)
		}
		user := testUser(t)
		work := filepath.Join(root, "work")
		src := filepath.Join(work, "src")
		nested := filepath.Join(src, "nested")
		dst := filepath.Join(work, "dst")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		wantFiles := map[string]string{
			"top.txt":         "hello selinux\n",
			"nested/deep.txt": "copied via rsync\n",
		}
		for rel, body := range wantFiles {
			if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range []string{work, src, nested, filepath.Join(src, "top.txt"), filepath.Join(src, "nested", "deep.txt")} {
			if err := chownToUser(p, user); err != nil {
				t.Fatal(err)
			}
		}
		rn := name(t)
		rlog := filepath.Join(t.TempDir(), "rsync.log")
		if err := Start(t.Context(), Spec{
			Name: rn, RootDir: root, Dir: "/work", ReadOnlyBinds: roBinds, User: user, Tmp: rootTmp(t, root), Log: rlog,
			Argv: []string{"rsync", "-a", "/work/src/", "/work/dst/"},
		}); err != nil {
			t.Fatal(err)
		}
		if s := waitExit(t, rn); s.ExitCode != 0 {
			rb, _ := os.ReadFile(rlog)
			t.Fatalf("rsync exited %+v; log:\n%s", s, rb)
		}
		for rel, want := range wantFiles {
			got, err := os.ReadFile(filepath.Join(dst, rel))
			if err != nil {
				t.Fatalf("read copied %s: %v", rel, err)
			}
			if string(got) != want {
				t.Errorf("copied %s = %q, want %q", rel, got, want)
			}
		}
	})
}

// A chrooted command and shell both receive a 0700 user-owned XDG_RUNTIME_DIR
// at /run/user/<uid> (on top of MountAPIVFS's /run) and a read-only
// /etc/containers/containers.conf.d/50-hotbed.conf so rootless podman and crun
// work without a systemd user session (board 10735).
func TestChrootRuntimeDirAndContainersConf(t *testing.T) {
	root, roBinds := minimalRoot(t)
	user := testUser(t)
	u, err := osuser.Lookup(user)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := chownToUser(filepath.Join(root, "work"), user); err != nil {
		t.Fatal(err)
	}
	// Make host /etc/subuid, /etc/subgid, /etc/containers/policy.json, and
	// /etc/containers/registries.conf available in minimalRoot when present so
	// rootless podman can initialize inside the minimal test chroot.
	for _, f := range []string{"/etc/subuid", "/etc/subgid", "/etc/containers/policy.json", "/etc/containers/registries.conf"} {
		if b, err := os.ReadFile(f); err == nil {
			dst := filepath.Join(root, f)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	tmpHost := rootTmp(t, root)
	runHost := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(runHost, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(runHost, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runHost, 0o700); err != nil {
		t.Fatal(err)
	}
	// Allow the non-root test user to traverse runHost's parent temp dir.
	if err := os.Chmod(filepath.Dir(runHost), 0o711); err != nil {
		t.Fatal(err)
	}

	confHost := filepath.Join(t.TempDir(), "containers.conf")
	const confBody = "[engine]\ncgroup_manager = \"cgroupfs\"\nevents_logger = \"file\"\n"
	if err := os.WriteFile(confHost, []byte(confBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(confHost), 0o711); err != nil {
		t.Fatal(err)
	}

	runTarget := fmt.Sprintf("/run/user/%d", uid)
	const confTarget = "/etc/containers/containers.conf.d/50-hotbed.conf"
	withConfRO := append(append([]Bind(nil), roBinds...), Bind{Source: confHost, Target: confTarget})

	// 1. Service unit (Start): /run/user/<uid> is 0700 owned by user, writable,
	//    and 50-hotbed.conf is readable and read-only.
	t.Run("service", func(t *testing.T) {
		n := name(t)
		log := filepath.Join(t.TempDir(), "out.log")
		if err := Start(t.Context(), Spec{
			Name:          n,
			RootDir:       root,
			Dir:           "/work",
			User:          user,
			Tmp:           tmpHost,
			Binds:         []Bind{{Source: runHost, Target: runTarget}},
			ReadOnlyBinds: withConfRO,
			Env:           []string{"HOME=/work", "XDG_RUNTIME_DIR=" + runTarget},
			Log:           log,
			Argv: []string{"sh", "-c", `set -eu
stat -c '%a %u' "$XDG_RUNTIME_DIR"
touch "$XDG_RUNTIME_DIR/probe"
grep -q 'cgroup_manager = "cgroupfs"' /etc/containers/containers.conf.d/50-hotbed.conf
if sh -c 'echo x >> /etc/containers/containers.conf.d/50-hotbed.conf' 2>/dev/null; then
  echo "ERROR: conf was writable" >&2
  exit 1
fi
echo service-ok`},
		}); err != nil {
			t.Fatal(err)
		}
		s := waitExit(t, n)
		b, _ := os.ReadFile(log)
		wantPrefix := fmt.Sprintf("700 %d\n", uid)
		if s.ExitCode != 0 || !strings.HasPrefix(string(b), wantPrefix) || !strings.Contains(string(b), "service-ok\n") {
			t.Fatalf("service exit=%+v, log:\n%s", s, b)
		}
	})

	// 2. Shell scope (StartShell): same /run/user/<uid> and read-only conf.
	t.Run("shell", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		h, err := StartShell(t.Context(), Spec{
			Name:          name(t),
			RootDir:       root,
			Dir:           "/work",
			User:          user,
			Tmp:           tmpHost,
			Binds:         []Bind{{Source: runHost, Target: runTarget}},
			ReadOnlyBinds: withConfRO,
			Env:           []string{"HOME=/work", "XDG_RUNTIME_DIR=" + runTarget},
			Argv: []string{"sh", "-c", `set -eu
stat -c '%a %u' "$XDG_RUNTIME_DIR"
grep -q 'cgroup_manager = "cgroupfs"' /etc/containers/containers.conf.d/50-hotbed.conf
echo shell-ok`},
		}, ShellIO{Stdout: &stdout, Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		code := h.Wait()
		wantPrefix := fmt.Sprintf("700 %d\n", uid)
		if code != 0 || !strings.HasPrefix(stdout.String(), wantPrefix) || !strings.Contains(stdout.String(), "shell-ok\n") {
			t.Fatalf("shell exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	// 3. Rootless podman & crun inside the chroot: without 50-hotbed.conf,
	//    podman warns about the missing systemd user session; with it +
	//    XDG_RUNTIME_DIR, podman info reports "cgroupfs file" cleanly and
	//    crun --version succeeds.
	t.Run("podman", func(t *testing.T) {
		if _, err := exec.LookPath("podman"); err != nil {
			t.Skipf("podman not installed: %v", err)
		}
		home := filepath.Join(root, "work", "podman-home")
		for _, sub := range []string{"", "data", "cache", "state"} {
			p := filepath.Join(home, sub)
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := chownToUser(p, user); err != nil {
				t.Fatal(err)
			}
		}
		baseEnv := []string{
			"HOME=/work/podman-home",
			"XDG_DATA_HOME=/work/podman-home/data",
			"XDG_CACHE_HOME=/work/podman-home/cache",
			"XDG_STATE_HOME=/work/podman-home/state",
		}

		// waitPodmanExit polls Status until the main podman command exits, then
		// stops the unit so podman's rootless pause process (which stays in the
		// unit's cgroup until KillMode=control-group teardown) does not block
		// cgroup.events Wait.
		waitPodmanExit := func(t *testing.T, unitName string) State {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				st, err := Status(t.Context(), unitName)
				if err != nil {
					t.Fatal(err)
				}
				if st.Exited {
					if err := Stop(t.Context(), unitName); err != nil {
						t.Fatalf("stop %s after exit: %v", unitName, err)
					}
					return st
				}
				time.Sleep(50 * time.Millisecond)
			}
			st, _ := Status(t.Context(), unitName)
			t.Fatalf("%s did not exit in 15s (state=%+v)", unitName, st)
			return State{}
		}

		// Copy this static test binary into a scratch build context so we can
		// execute a real `podman build` RUN step offline (no image pulls).
		exePath, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		exeBytes, err := os.ReadFile(exePath)
		if err != nil {
			t.Fatal(err)
		}
		ctxDir := filepath.Join(root, "work", "buildctx")
		if err := os.MkdirAll(ctxDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctxDir, "testbin"), exeBytes, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctxDir, "Containerfile"), []byte("FROM scratch\nCOPY testbin /testbin\nRUN [\"/testbin\", \"-test.run=^$\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{ctxDir, filepath.Join(ctxDir, "testbin"), filepath.Join(ctxDir, "Containerfile")} {
			if err := chownToUser(p, user); err != nil {
				t.Fatal(err)
			}
		}

		// With 50-hotbed.conf and XDG_RUNTIME_DIR, podman info reports exact
		// "cgroupfs file" with zero warnings, crun (when installed) succeeds,
		// and a real `podman build` RUN step succeeds inside the command's own
		// cgroup without a systemd user session (board 10735).
		nGood := name(t)
		logGood := filepath.Join(t.TempDir(), "good.log")
		if err := Start(t.Context(), Spec{
			Name:          nGood,
			RootDir:       root,
			Dir:           "/work",
			User:          user,
			Tmp:           tmpHost,
			Binds:         []Bind{{Source: runHost, Target: runTarget}},
			ReadOnlyBinds: withConfRO,
			TraceFS:       true,
			Env:           append(baseEnv, "XDG_RUNTIME_DIR="+runTarget),
			Log:           logGood,
			Argv: []string{"sh", "-c", `set -eu
if command -v crun >/dev/null 2>&1; then crun --version >/dev/null; fi
info="$(podman info --format '{{.Host.CgroupManager}} {{.Host.EventLogger}}')"
test "$info" = "cgroupfs file"
podman build --network=none -q /work/buildctx >/dev/null 2>&1
echo podman-build-ok`},
		}); err != nil {
			t.Fatal(err)
		}
		s := waitPodmanExit(t, nGood)
		goodOut, _ := os.ReadFile(logGood)
		if s.ExitCode != 0 || strings.TrimSpace(string(goodOut)) != "podman-build-ok" {
			t.Fatalf("fixed chroot podman exit=%+v, output=%q; want exit 0 and %q", s, string(goodOut), "podman-build-ok")
		}
	})
}
