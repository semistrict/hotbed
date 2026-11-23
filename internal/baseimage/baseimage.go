// Package baseimage builds and caches hotbed's shared Linux rootfs base image
// as a read-only copy-on-write snapshot in storage.Store.
//
// Cold project bases clone from this snapshot instead of starting from an empty
// directory against the host filesystem, so workloads and active tracers run in
// a deterministic chroot with zero host-installed toolchains or tracing packages.
package baseimage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/storage"
)

const (
	// DefaultImage is the minimal Debian OCI image unpacked before running the
	// base provisioning script, pinned by its multi-arch (amd64 + arm64) OCI
	// index digest. Public ECR avoids Docker Hub anonymous pull limits, and
	// Debian's linux-perf package installs a real /usr/bin/perf ELF without
	// Ubuntu's uname -r wrapper script.
	DefaultImage = "public.ecr.aws/debian/debian:trixie-slim@sha256:ee2f5ba823fe4f46fcfcf19fc0ab46361ba48f04078cf2f8921f4d8e9b1947e9"

	// WorkDir is the workspace directory inside the rootfs.
	WorkDir = "/work"

	// ReadyMarker is written into the base-image volume immediately before
	// snapshotting so a worker can tell a finished base image from one left
	// behind by an interrupted build.
	ReadyMarker = ".hotbed-baseimage-ready"
)

// Spec describes one shared worker base rootfs image. Every field participates
// in Key(), so changing the image ref, architecture, user identity, or
// provisioning script produces a new snapshot name automatically.
type Spec struct {
	Image  string // OCI image reference; defaults to DefaultImage
	Arch   string // Linux GOARCH ("amd64" or "arm64"); defaults to runtime.GOARCH
	User   string // non-root workspace user created inside the rootfs
	UID    int    // numeric UID inside the rootfs (matches host command user)
	GID    int    // numeric GID inside the rootfs (matches host command user)
	Home   string // user home directory inside the rootfs; defaults to /work/.hotbed/home
	Script string // root provisioning script; defaults to DefaultProvisionScript()
}

// SudoersFile grants the build user passwordless sudo to root inside the
// chrooted rootfs.
const SudoersFile = "/etc/sudoers.d/hotbed-user"

// Validate rejects root or invalid user identities before Normalized fills
// zero-value defaults.
func (s Spec) Validate() error {
	if s.User == "root" {
		return errors.New(`baseimage: user "root" is not allowed; commands must run as a non-root user`)
	}
	if s.UID < 0 {
		return fmt.Errorf("baseimage: invalid uid %d", s.UID)
	}
	if s.GID < 0 {
		return fmt.Errorf("baseimage: invalid gid %d", s.GID)
	}
	if s.User != "" && s.UID == 0 {
		return fmt.Errorf("baseimage: user %q has uid 0 (root); commands must run as a non-root user", s.User)
	}
	return nil
}

// Normalized returns a copy of s with defaults filled in.
func (s Spec) Normalized() Spec {
	if s.Image == "" {
		s.Image = DefaultImage
	}
	if s.Arch == "" {
		s.Arch = runtime.GOARCH
	}
	if s.User == "" {
		s.User = "hotbed"
	}
	if s.UID == 0 {
		s.UID = 1000
	}
	if s.GID == 0 {
		s.GID = s.UID
	}
	if s.Home == "" {
		s.Home = WorkDir + "/.hotbed/home"
	}
	if s.Script == "" {
		s.Script = DefaultProvisionScript(s.User, s.UID, s.GID, s.Home)
	}
	return s
}

// Key is the deterministic storage snapshot name for this spec ("baseimg-<hash12>").
func (s Spec) Key() string {
	n := s.Normalized()
	h := sha256.New()
	fmt.Fprintf(h, "v1\nimage=%s\narch=%s\nuser=%s\nuid=%d\ngid=%d\nhome=%s\nscript=%s\n",
		n.Image, n.Arch, n.User, n.UID, n.GID, n.Home, n.Script)
	return "baseimg-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Volume is the storage volume name backing this spec's snapshot.
func (s Spec) Volume() string {
	return "baseimages/" + s.Key()
}

// DefaultProvisionScript returns the root provisioning script executed inside
// the freshly unpacked Debian rootfs. It installs core build utilities, all
// five Linux tracing tools (strace, wrapper-free perf, bpftrace, eu-stack via
// elfutils, and gdb), and latest stable Rust via rustup inside /work/.hotbed so
// project bases start ready and can install extra toolchains non-root. The
// build user is granted passwordless sudo to root (SudoersFile) so setup steps
// and commands can install OS packages and run root operations inside the
// chroot.
func DefaultProvisionScript(user string, uid, gid int, home string) string {
	sudoers := fmt.Sprintf(`
# Grant the build user passwordless sudo to root inside the chroot.
printf '#%%d ALL=(ALL:ALL) NOPASSWD: ALL\n' %[1]d > %[2]s
chmod 0440 %[2]s
visudo -cq
`, uid, SudoersFile)
	return fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  bash ca-certificates coreutils curl git gh openssh-client \
  build-essential clang pkg-config python3 gzip tar xz-utils unzip jq procps iproute2 passwd \
  libseccomp-dev libssl-dev libelf-dev musl-tools sqlite3 fuse3 sudo \
  strace linux-perf bpftrace elfutils gdb
rm -rf /var/lib/apt/lists/*

# Ensure /usr/bin/perf is a direct executable (Debian's linux-perf installs it
# directly, or as /usr/bin/perf_<ver>; fall back to symlinking if needed).
if ! command -v perf >/dev/null 2>&1; then
  real_perf="$(ls -1 /usr/bin/perf_* 2>/dev/null | sort -V | tail -n 1 || true)"
  if [ -n "$real_perf" ]; then
    ln -sf "$real_perf" /usr/bin/perf
  fi
fi

# Create the non-root workspace user matching the worker's configured uid/gid.
if ! getent group %[3]d >/dev/null 2>&1; then
  groupadd -g %[3]d %[1]s
fi
grp="$(getent group %[3]d | cut -d: -f1)"
if ! getent passwd %[2]d >/dev/null 2>&1; then
  useradd -M -u %[2]d -g "$grp" -d %[4]s -s /bin/bash %[1]s
fi

mkdir -p /proc /sys /dev /run /tmp /var/tmp /etc \
  /work/.hotbed/bin /work/.hotbed/cargo /work/.hotbed/rustup \
  /work/.hotbed/go /work/.hotbed/cache %[4]s
chmod 1777 /tmp /var/tmp
# Ensure /etc/mtab points to /proc/self/mounts: minimal Debian OCI images
# omit it because systemd tmpfiles never boots them, causing fusermount3 to
# fail opening /etc/mtab or trying to lock /etc/mtab~ (board 7975).
ln -snf /proc/self/mounts /etc/mtab
chown -R %[2]d:%[3]d /work

# Install rustup + latest stable Rust into /work/.hotbed using the pinned
# rustup-init binary verified by SHA-256 (avoiding unverified curl | sh) so
# every cloned base inherits it and project steps can add toolchains or targets
# without root.
su -s /bin/bash %[1]s -c '
  set -euo pipefail
  export HOME=%[4]s
  export RUSTUP_HOME=/work/.hotbed/rustup
  export CARGO_HOME=/work/.hotbed/cargo
  case "$(uname -m)" in
    x86_64)  triple=x86_64-unknown-linux-gnu;  sha=b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55 ;;
    aarch64) triple=aarch64-unknown-linux-gnu; sha=9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282 ;;
    *) echo "unsupported arch for rustup-init: $(uname -m)" >&2; exit 1 ;;
  esac
  tmp="$(mktemp /tmp/rustup-init.XXXXXX)"
  curl --proto "=https" --tlsv1.2 -fsSL "https://static.rust-lang.org/rustup/archive/1.29.3/${triple}/rustup-init" -o "$tmp"
  printf "%%s  %%s\n" "$sha" "$tmp" | sha256sum -c -
  chmod 0755 "$tmp"
  "$tmp" -y --no-modify-path --default-toolchain stable --component clippy,rustfmt
  rm -f "$tmp"
'

# Fail hard if any required baseline binary or tracer is missing.
export PATH="/work/.hotbed/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
for bin in git curl gcc pkg-config sudo strace perf bpftrace eu-stack gdb rustup cargo rustc; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing required tool in base image: $bin" >&2; exit 1; }
done
%[5]s`, shellQuote(user), uid, gid, shellQuote(home), sudoers)
}

// Puller unpacks an OCI image for (ref, arch) into rootDir.
type Puller func(ctx context.Context, imageRef, arch, rootDir string) error

// Provisioner runs the provisioning script of image key as root inside
// rootDir. A provisioning outlives the worker process that started it (a
// worker re-execs into a new build mid-way), so the next build of key
// abandons whatever one is left before it clears the volume that one is using.
type Provisioner interface {
	Provision(ctx context.Context, key, rootDir, script string) error
	// Abandon stops a provisioning of key that is still running, if any.
	Abandon(ctx context.Context, key string) error
}

// ProvisionFunc is a Provisioner that leaves nothing running behind it.
type ProvisionFunc func(ctx context.Context, rootDir, script string) error

func (f ProvisionFunc) Provision(ctx context.Context, _, rootDir, script string) error {
	return f(ctx, rootDir, script)
}

func (ProvisionFunc) Abandon(context.Context, string) error { return nil }

// Manager ensures the worker's shared base rootfs snapshot exists in Store,
// pulling and provisioning it once and reusing it on subsequent calls.
type Manager struct {
	Store        storage.Store
	Spec         Spec
	Pull         Puller        // defaults to PullOCI
	Provision    Provisioner   // required when Spec.Script != "" (defaults to no-op only if explicitly nil-safe in tests)
	BuildTimeout time.Duration // defaults to 15*time.Minute

	// unmount and retryDelay are test hooks; nil/zero use production defaults.
	unmount    func(rootDir string) error
	retryDelay time.Duration

	mu          sync.Mutex
	inFlight    *ensureCall
	verifiedKey string // snapshot key verified present in Store (guarded by mu)
	wg          sync.WaitGroup
}

// Wait blocks until all in-flight detached base-image builds started by Ensure
// have finished. Callers should invoke Wait during shutdown or test cleanup
// once no new Ensure calls will be started.
func (m *Manager) Wait() {
	m.wg.Wait()
}

type ensureCall struct {
	done chan struct{}
	snap string
	root string
	err  error
}

func (m *Manager) snapshotExists(ctx context.Context, key string) (bool, error) {
	snaps, err := m.Store.ListSnapshots(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(snaps, key), nil
}

// isReadyLocked reports whether rootDir carries ReadyMarker for key and the
// backing snapshot key exists in m.Store. Once built or verified, snapshot
// existence is cached in m.verifiedKey so hot-path lookups do not query
// storage on every call. Call with m.mu held.
func (m *Manager) isReadyLocked(ctx context.Context, rootDir, key string) (bool, error) {
	if !isReady(rootDir, key) {
		if m.verifiedKey == key {
			m.verifiedKey = ""
		}
		return false, nil
	}
	if m.verifiedKey == key {
		return true, nil
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	ok, err := m.snapshotExists(ctx, key)
	if err != nil {
		return false, err
	}
	if ok {
		m.verifiedKey = key
	}
	return ok, nil
}

// ReadyRootDir returns the on-disk path of the base-image volume if it has
// already been built, marked ready, and snapshotted, or "" otherwise. It never
// blocks on a pull or build, so Capabilities() can inspect it cheaply.
func (m *Manager) ReadyRootDir() string {
	if m.Store == nil {
		return ""
	}
	if err := m.Spec.Validate(); err != nil {
		return ""
	}
	spec := m.Spec.Normalized()
	key := spec.Key()
	rootDir := m.Store.Path(spec.Volume())

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight != nil {
		return ""
	}
	ready, err := m.isReadyLocked(nil, rootDir, key)
	if err != nil || !ready {
		return ""
	}
	return rootDir
}

// Ensure returns the snapshot name and on-disk rootfs path of the shared base
// image, building and snapshotting it on the first call. Concurrent callers
// share a single build, and a single caller's timeout does not abort the shared
// build in progress.
//
// Its results are not named: the build, which may outlive this call, reads
// what it is given, never what a return here writes.
func (m *Manager) Ensure(ctx context.Context) (string, string, error) {
	if err := m.Spec.Validate(); err != nil {
		return "", "", err
	}
	spec := m.Spec.Normalized()
	key := spec.Key()
	vol := spec.Volume()
	rootDir := m.Store.Path(vol)

	m.mu.Lock()
	call := m.inFlight
	if call == nil {
		ready, rerr := m.isReadyLocked(ctx, rootDir, key)
		if rerr != nil {
			m.mu.Unlock()
			return "", "", fmt.Errorf("check base-image snapshot %s: %w", key, rerr)
		}
		if ready {
			m.mu.Unlock()
			return key, rootDir, nil
		}
		call = &ensureCall{done: make(chan struct{})}
		m.inFlight = call
		timeout := m.BuildTimeout
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer cancel()
			snap, root, berr := m.build(buildCtx, spec, key, vol, rootDir)
			call.snap, call.root, call.err = snap, root, berr
			m.mu.Lock()
			if berr == nil {
				m.verifiedKey = key
			} else if m.verifiedKey == key {
				m.verifiedKey = ""
			}
			if m.inFlight == call {
				m.inFlight = nil
			}
			m.mu.Unlock()
			close(call.done)
		}()
	}
	m.mu.Unlock()

	select {
	case <-call.done:
		return call.snap, call.root, call.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (m *Manager) build(ctx context.Context, spec Spec, key, vol, rootDir string) (string, string, error) {
	pull := m.Pull
	if pull == nil {
		pull = PullOCI
	}
	if m.Provision == nil {
		return "", "", errors.New("baseimage: no provisioner configured")
	}

	// Clear any partial volume or snapshot left by an interrupted earlier
	// build, once nothing of that build still runs in it.
	if err := m.Provision.Abandon(ctx, key); err != nil {
		return "", "", fmt.Errorf("stop leftover provisioning of base image %s: %w", key, err)
	}
	if err := m.Store.DestroySnapshot(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", "", fmt.Errorf("clear leftover base-image snapshot %s: %w", key, err)
	}
	if err := m.destroyVolume(ctx, vol, rootDir); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", "", fmt.Errorf("clear leftover base-image volume %s: %w", vol, err)
	}

	if err := m.Store.Create(ctx, vol); err != nil {
		return "", "", fmt.Errorf("create base-image volume %s: %w", vol, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			cleanCtx := context.WithoutCancel(ctx)
			if aerr := m.Provision.Abandon(cleanCtx, key); aerr != nil {
				slog.Warn("abandon failed base-image provisioning", "key", key, "err", aerr)
			}
			if derr := m.destroyVolume(cleanCtx, vol, rootDir); derr != nil && !errors.Is(derr, storage.ErrNotFound) {
				slog.Warn("clean up failed base-image volume", "volume", vol, "err", derr)
			}
		}
	}()

	if err := labelRootfs(rootDir); err != nil {
		return "", "", fmt.Errorf("label base-image rootfs %s: %w", rootDir, err)
	}

	slog.Info("pulling base rootfs image", "image", spec.Image, "arch", spec.Arch, "snapshot", key)
	if err := pull(ctx, spec.Image, spec.Arch, rootDir); err != nil {
		return "", "", fmt.Errorf("pull base image %s (%s): %w", spec.Image, spec.Arch, err)
	}
	if err := prepareMountpoints(rootDir); err != nil {
		return "", "", fmt.Errorf("prepare chroot mountpoints in %s: %w", rootDir, err)
	}

	slog.Info("provisioning base rootfs image", "snapshot", key)
	if err := m.Provision.Provision(ctx, key, rootDir, spec.Script); err != nil {
		return "", "", fmt.Errorf("provision base image %s: %w", key, err)
	}
	if err := writeReadyMarker(rootDir, key); err != nil {
		return "", "", fmt.Errorf("write base-image ready marker: %w", err)
	}
	if err := m.Store.Snapshot(ctx, vol, key); err != nil {
		return "", "", fmt.Errorf("snapshot base image %s: %w", key, err)
	}
	cleanup = false
	slog.Info("base rootfs image ready", "snapshot", key, "root", rootDir)
	return key, rootDir, nil
}

// destroyVolume unmounts any leftover mounts under rootDir and destroys vol,
// retrying with bounded exponential backoff if the kernel transiently holds
// the dataset busy while mount namespaces finish tearing down (board 7965).
func (m *Manager) destroyVolume(ctx context.Context, vol, rootDir string) error {
	unmount := m.unmount
	if unmount == nil {
		unmount = unmountSubmounts
	}
	if err := unmount(rootDir); err != nil {
		slog.Warn("unmount base-image submounts", "volume", vol, "root", rootDir, "err", err)
	}

	const maxAttempts = 8
	delay := m.retryDelay
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	var lastErr error
	for attempt := range maxAttempts {
		err := m.Store.Destroy(ctx, vol)
		if err == nil || errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if !errors.Is(err, storage.ErrBusy) {
			return err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		slog.Warn("base-image volume busy; retrying destroy", "volume", vol, "attempt", attempt+1, "err", err)
		if uerr := unmount(rootDir); uerr != nil {
			slog.Warn("unmount base-image submounts before retry", "volume", vol, "root", rootDir, "err", uerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
	return lastErr
}

// prepareMountpoints ensures the standard API VFS mountpoints and a regular
// /etc/resolv.conf target exist inside rootDir before systemd enters it.
func prepareMountpoints(rootDir string) error {
	rfs, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer rfs.Close()

	for _, d := range []string{"proc", "sys", "dev", "run", "tmp", "var/tmp", "etc", "work"} {
		if err := mkdirAllNoSymlink(rfs, d); err != nil {
			return err
		}
	}
	for _, d := range []string{"tmp", "var/tmp"} {
		if err := rfs.Chmod(d, 0o1777); err != nil {
			return err
		}
	}
	// Replace any dangling symlink at /etc/resolv.conf with an empty regular
	// file so systemd BindReadOnlyPaths=/etc/resolv.conf has a valid target.
	const resolv = "etc/resolv.conf"
	if fi, err := rfs.Lstat(resolv); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := rfs.Remove(resolv); err != nil {
			return err
		}
	}
	f, err := rfs.OpenFile(resolv, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

const maxReadyMarkerBytes = 256

// writeReadyMarker atomically writes ReadyMarker inside rootDir using an
// os.Root handle so an image-supplied symlink at ReadyMarker is replaced rather
// than followed outside rootDir.
func writeReadyMarker(rootDir, key string) error {
	rfs, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer rfs.Close()

	var tmpName string
	var f *os.File
	for range 8 {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		candidate := ReadyMarker + ".tmp." + hex.EncodeToString(rnd[:])
		f, err = rfs.OpenFile(candidate, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		tmpName = candidate
		break
	}
	if f == nil {
		return fmt.Errorf("create temp ready marker: %w", os.ErrExist)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = rfs.Remove(tmpName)
		}
	}()

	if _, err := io.WriteString(f, key+"\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o444); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Rename replaces symlinks and regular files atomically, but fails with
	// EISDIR if an image placed a real directory at ReadyMarker.
	if fi, err := rfs.Lstat(ReadyMarker); err == nil && fi.IsDir() {
		if err := rfs.RemoveAll(ReadyMarker); err != nil {
			return err
		}
	}
	if err := rfs.Rename(tmpName, ReadyMarker); err != nil {
		return err
	}
	cleanupTmp = false
	return nil
}

// ReadReadyMarker reads ReadyMarker inside rootDir using an os.Root handle
// without following symlinks, without blocking on special files (FIFOs,
// sockets, devices), and with a strict size limit.
func ReadReadyMarker(rootDir string) (string, error) {
	if rootDir == "" {
		return "", os.ErrNotExist
	}
	rfs, err := os.OpenRoot(rootDir)
	if err != nil {
		return "", err
	}
	defer rfs.Close()

	lfi, err := rfs.Lstat(ReadyMarker)
	if err != nil {
		return "", err
	}
	if !lfi.Mode().IsRegular() {
		return "", fmt.Errorf("base-image ready marker %s is not a regular file", ReadyMarker)
	}
	f, err := rfs.OpenFile(ReadyMarker, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()

	ffi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !ffi.Mode().IsRegular() || !os.SameFile(lfi, ffi) {
		return "", fmt.Errorf("base-image ready marker %s changed while reading", ReadyMarker)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReadyMarkerBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxReadyMarkerBytes {
		return "", fmt.Errorf("base-image ready marker %s exceeds %d bytes", ReadyMarker, maxReadyMarkerBytes)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("base-image ready marker %s is empty", ReadyMarker)
	}
	return key, nil
}

// MatchesKey reports whether dir was built from the base image identified by key.
func MatchesKey(dir, key string) bool {
	if dir == "" || key == "" {
		return false
	}
	return isReady(dir, key)
}

func isReady(rootDir, key string) bool {
	if rootDir == "" || key == "" {
		return false
	}
	got, err := ReadReadyMarker(rootDir)
	return err == nil && got == key
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
