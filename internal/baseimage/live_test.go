//go:build linux && systemdtest

// Run as root on a systemd Linux host with network access:
//
//	sudo HOTBED_TEST_USER=$USER go test -v -tags systemdtest -run TestLiveBaseImageAndTracers ./internal/baseimage/
//
// The test's store is plain directories copied in full (fake.Store), three
// images' worth: where /tmp is a small tmpfs, set HOTBED_TEST_DIR=/var/tmp.
package baseimage_test

import (
	"cmp"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/unit"
)

func TestLiveBaseImageAndTracers(t *testing.T) {
	uName := os.Getenv("HOTBED_TEST_USER")
	if uName == "" {
		t.Fatal("set HOTBED_TEST_USER to the non-root user")
	}
	u, err := user.Lookup(uName)
	if err != nil {
		t.Fatalf("lookup user %q: %v", uName, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// Store under /tmp (or HOTBED_TEST_DIR) so systemd RootDirectory can
	// traverse all parent dirs (0755).
	baseDir, err := os.MkdirTemp(cmp.Or(os.Getenv("HOTBED_TEST_DIR"), "/tmp"), "hotbed-baseimg-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })
	if err := os.Chmod(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store := &fake.Store{Root: filepath.Join(baseDir, "store")}
	logDir := filepath.Join(baseDir, "logs")
	mgr := &baseimage.Manager{
		Store: store,
		Spec: baseimage.Spec{
			User: uName,
			UID:  uid,
			GID:  gid,
		},
		Provision: baseimage.SystemdProvisioner(unit.DefaultSlice, logDir),
	}
	t.Cleanup(mgr.Wait)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	snap, rootDir, err := mgr.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure base image: %v", err)
	}
	t.Logf("built shared base image snapshot=%s rootDir=%s", snap, rootDir)

	// 1. Verify all 5 tracing tools are discovered inside the base image rootfs.
	insp := inspect.Linux{BaseRootDir: mgr.ReadyRootDir}
	caps := insp.Capabilities()
	if len(caps) != 5 {
		t.Fatalf("got %d capabilities, want 5", len(caps))
	}
	for _, c := range caps {
		if !c.Available {
			t.Errorf("tracer %s unavailable in base image: %s", c.Name, c.Detail)
		}
	}

	// 2. Run a non-root workload inside the chroot and verify rustup, cargo,
	//    strace, perf, eu-stack, gdb, bpftrace, CAP_PERFMON, and passwordless
	//    sudo to real root (including installing an OS package). The user,
	//    not root, then mounts a kernel FUSE filesystem (bindfs) and unmounts
	//    it with fusermount3 -u, which fails without the image's /etc/mtab
	//    ("unable to open fuse lock file", board 8613).
	tmpHost := filepath.Join(rootDir, ".tmp", "live")
	t.Cleanup(func() { os.RemoveAll(filepath.Join(rootDir, ".tmp")) })
	if err := os.MkdirAll(tmpHost, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpHost, 0o1777); err != nil {
		t.Fatal(err)
	}
	unitName := "hotbed-baseimg-smoke"
	unitLog := filepath.Join(logDir, unitName+".log")
	t.Cleanup(func() { _ = unit.Stop(context.WithoutCancel(t.Context()), unitName) })

	if err := unit.Start(ctx, unit.Spec{
		Name:         unitName,
		RootDir:      rootDir,
		Dir:          "/work",
		User:         uName,
		Tmp:          tmpHost,
		PathPrefix:   []string{"/work/.hotbed/bin", "/work/.hotbed/cargo/bin"},
		Capabilities: []string{"CAP_PERFMON"},
		TraceFS:      true,
		Env:          []string{"HOME=/work/.hotbed/home", "RUSTUP_HOME=/work/.hotbed/rustup", "CARGO_HOME=/work/.hotbed/cargo"},
		Log:          unitLog,
		Argv: []string{"bash", "-c", `
			set -euo pipefail
			echo "uid=$(id -u) sudo=$(sudo -n id -u)"
			rustc --version
			cargo --version
			strace -f -c -- /bin/true >/dev/null 2>&1
			perf --version
			perf stat -e task-clock -- /bin/true >/dev/null 2>&1
			eu-stack --version
			gdb --version | head -n 1
			bpftrace --version
			sudo -n apt-get update -qq >/dev/null
			sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends tree bindfs >/dev/null
			tree --version >/dev/null
			mkdir "$TMPDIR/fuse-src" "$TMPDIR/fuse-mnt"
			echo fused > "$TMPDIR/fuse-src/f"
			bindfs --no-allow-other "$TMPDIR/fuse-src" "$TMPDIR/fuse-mnt"
			[ "$(cat "$TMPDIR/fuse-mnt/f")" = fused ]
			fusermount3 -u "$TMPDIR/fuse-mnt"
			! grep -qF " $TMPDIR/fuse-mnt " /proc/self/mounts
			echo x > "$TMPDIR/tmp-ok"
			echo "ALL_IN_CHROOT_OK"
		`},
	}); err != nil {
		t.Fatalf("start chroot smoke unit: %v", err)
	}
	if err := unit.Wait(ctx, unit.DefaultSlice, unitName); err != nil {
		t.Fatalf("wait smoke unit: %v", err)
	}
	st, err := unit.Status(ctx, unitName)
	if err != nil {
		t.Fatal(err)
	}
	outBytes, _ := os.ReadFile(unitLog)
	if want := "uid=" + u.Uid + " sudo=0\n"; !st.Exited || st.ExitCode != 0 || !strings.HasPrefix(string(outBytes), want) || !strings.Contains(string(outBytes), "ALL_IN_CHROOT_OK") {
		t.Fatalf("chroot smoke failed (state=%+v):\n%s", st, outBytes)
	}
	t.Logf("chroot smoke output:\n%s", outBytes)
	if b, err := os.ReadFile(filepath.Join(tmpHost, "tmp-ok")); err != nil || string(b) != "x\n" {
		t.Fatalf("its /tmp is not its Tmp %s: %q %v", tmpHost, b, err)
	}
	fi, err := os.Stat(filepath.Join(rootDir, "usr", "bin", "tree"))
	if err != nil {
		t.Fatalf("the package sudo installed in the chroot is not in the rootfs: %v", err)
	}
	if owner := fi.Sys().(*syscall.Stat_t).Uid; owner != 0 {
		t.Errorf("/usr/bin/tree is owned on disk by %d, want 0", owner)
	}

	// 3. Verify inspect.Linux executes eu-stack, strace, perf stat, and bpftrace
	//    FROM the base image rootfs against a live systemd unit inside the chroot.
	tracedUnit := "hotbed-baseimg-traced"
	tracedLog := filepath.Join(logDir, tracedUnit+".log")
	t.Cleanup(func() { _ = unit.Stop(context.WithoutCancel(t.Context()), tracedUnit) })
	if err := unit.Start(ctx, unit.Spec{
		Name:    tracedUnit,
		RootDir: rootDir,
		Dir:     "/work",
		User:    uName,
		Tmp:     tmpHost,
		Log:     tracedLog,
		Argv:    []string{"sh", "-c", "while true; do cat /etc/hostname >/dev/null; sleep 0.2; done"},
	}); err != nil {
		t.Fatalf("start traced unit: %v", err)
	}
	var tracedSt unit.State
	for range 20 {
		tracedSt, err = unit.Status(ctx, tracedUnit)
		if err == nil && tracedSt.PID > 0 && tracedSt.Cgroup != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if tracedSt.PID == 0 || tracedSt.Cgroup == "" {
		t.Fatalf("traced unit did not start cleanly: %+v %v", tracedSt, err)
	}
	target := inspect.Target{
		CommandID: "live-smoke",
		UnitName:  tracedUnit,
		Cgroup:    tracedSt.Cgroup,
		MainPID:   tracedSt.PID,
	}

	for _, kind := range []pb.TraceKind{
		pb.TraceKind_TRACE_KIND_STACKS,
		pb.TraceKind_TRACE_KIND_STRACE_SUMMARY,
		pb.TraceKind_TRACE_KIND_PERF_STAT,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
	} {
		var final *pb.TraceSummary
		err := insp.Trace(ctx, target, &pb.TraceCommandRequest{
			Id:              "live-smoke",
			Kind:            kind,
			DurationSeconds: 2,
		}, func(ch *pb.TraceCommandChunk) error {
			if ch.Done {
				final = ch.Summary
			}
			return nil
		})
		if err != nil {
			t.Errorf("in-rootfs Trace(%v) failed: %v", kind, err)
			continue
		}
		if final == nil || strings.TrimSpace(final.Raw) == "" {
			t.Errorf("in-rootfs Trace(%v) returned empty output", kind)
		} else {
			t.Logf("in-rootfs Trace(%v) ok (%d bytes)", kind, len(final.Raw))
		}
	}
}
