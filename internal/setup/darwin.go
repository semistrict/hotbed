//go:build darwin

package setup

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/shell"
	"github.com/semistrict/hotbed/internal/worker"
)

func init() {
	watchPausedVM = darwinWatchPausedVM
}

// Setup creates, boots, and provisions a QEMU Ubuntu Linux worker VM in o.Dir
// (default ~/.hotbed) with sparse discard-enabled qcow2 + ZFS storage, joins
// it to o.Controller, and starts the hotbed-worker systemd service inside it.
func Setup(ctx context.Context, o Options, _ worker.Tailnet, hc *http.Client, _ worker.Check) (Result, error) {
	if o.ZFSDevice != "" {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "--zfs-device is only supported on Linux workers")
	}
	controller, err := api.ControllerHostPort(o.Controller)
	if err != nil {
		return Result{}, err
	}
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return Result{}, err
	}
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return Result{}, api.Errorf(api.CodePlatformNotSupported, "macOS worker VM supports arm64 and amd64; this is %s", arch)
	}

	qemuBin, qemuImg, err := findQEMUBinaries(arch)
	if err != nil {
		return Result{}, err
	}
	fwCode, fwVarsTemplate, err := findUEFIFirmware(arch, qemuBin)
	if err != nil {
		return Result{}, err
	}

	p := NewVMPaths(baseDir)
	if err := os.MkdirAll(p.CacheDir, 0o755); err != nil {
		return Result{}, err
	}
	if err := ensureVMPrivatePermissions(p); err != nil {
		return Result{}, err
	}

	// Resolve or download the Linux hotbed binary up front so we fail fast
	// before downloading an image or booting a VM.
	linuxBin, err := resolveLinuxBinary(ctx, o.BinaryPath, controller, p.CacheDir, arch, hc, o.Progress)
	if err != nil {
		return Result{}, err
	}

	// Load existing VM config if re-running setup idempotently.
	var prev VMConfig
	if readJSON(p.Config, &prev) != nil {
		prev = VMConfig{}
	}

	username := cmp.Or(o.User, prev.User)
	if username == "" {
		username, err = DefaultUser()
		if err != nil {
			return Result{}, err
		}
	}
	hostname := SanitizeHostname(cmp.Or(o.Hostname, prev.Hostname, DefaultVMHostname()))
	ubuntuVer := cmp.Or(o.UbuntuVersion, prev.UbuntuVersion, DefaultUbuntuVersion)
	cpus := cmp.Or(max(0, o.CPUs), max(0, prev.CPUs), defaultVMCPUs())
	memBytes := cmp.Or(max(0, o.MemoryBytes), max(0, prev.MemoryBytes))
	if memBytes == 0 {
		if memBytes, err = defaultVMMemory(); err != nil {
			return Result{}, err
		}
	}
	rootSize := cmp.Or(max(0, o.RootSize), max(0, prev.RootSizeBytes), DefaultRootSize)
	zfsSize := cmp.Or(max(0, o.DiskSize), max(0, prev.ZFSSizeBytes), DefaultDiskSize)
	swapSize := cmp.Or(max(0, o.SwapSize), max(0, prev.SwapSizeBytes), memBytes)
	sshPort := cmp.Or(max(0, o.SSHPort), max(0, prev.SSHPort))
	if sshPort == 0 {
		sshPort, err = pickSSHPort()
		if err != nil {
			return Result{}, err
		}
	}

	imageURL, imageFilename, sumsURL := UbuntuCloudImageURL(ubuntuVer, arch)
	if o.ImageURL != "" {
		imageURL = o.ImageURL
		imageFilename = filepath.Base(imageURL)
		sumsURL = ""
	}

	cfg := VMConfig{
		Controller:     controller,
		User:           username,
		Hostname:       hostname,
		Arch:           arch,
		UbuntuVersion:  ubuntuVer,
		ImageURL:       imageURL,
		CPUs:           cpus,
		MemoryBytes:    memBytes,
		RootSizeBytes:  rootSize,
		ZFSSizeBytes:   zfsSize,
		SwapSizeBytes:  swapSize,
		SSHPort:        sshPort,
		QEMUBinary:     qemuBin,
		FirmwareCode:   fwCode,
		StateDir:       DefaultVMStateDir,
		UnitProperties: o.UnitProperties,
		Slice:          o.Slice,
		Autostart:      !o.NoAutostart,
		SleepAfter:     cmp.Or(o.SleepAfter, prev.SleepAfter),
	}
	sleepAfter, err := cfg.SleepAfterDuration()
	if err != nil {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "--sleep-after: %v", err)
	}
	if err := refuseResizeWhileRunning(ctx, prev, cfg, p); err != nil {
		return Result{}, err
	}
	// Set up, the VM is not the supervisor's to stop: it starts again, if
	// it is to, once the VM is.
	if err := stopSupervisor(ctx, cfg, p); err != nil {
		return Result{}, err
	}

	progressf(o.Progress, "preparing Ubuntu %s (%s) worker VM %q in %s (%d vCPUs, %s RAM, %s sparse ZFS)",
		ubuntuVer, arch, hostname, baseDir, cpus, FormatBytes(memBytes), FormatBytes(zfsSize))

	cachedImg, err := ensureCloudImage(ctx, p.CacheDir, imageURL, imageFilename, sumsURL, o.Progress)
	if err != nil {
		return Result{}, err
	}

	signer, pubKey, err := ensureSSHKeypair(p.KeyPriv, p.KeyPub)
	if err != nil {
		return Result{}, err
	}
	pubKeys := append([]string{pubKey}, hostPublicKeys()...)

	hostKey, err := prepareVMHostKeyForSetup(p)
	if err != nil {
		return Result{}, err
	}
	if err := ensureSeedISO(ctx, p.SeedISO, hostname, username, pubKeys, hostKey); err != nil {
		return Result{}, err
	}
	if err := ensureVMDisks(ctx, qemuImg, cachedImg, fwVarsTemplate, cfg, p, o.Progress); err != nil {
		return Result{}, err
	}
	if err := ensureVMPrivatePermissions(p); err != nil {
		return Result{}, err
	}
	if err := writeJSONAtomic(p.Config, cfg, 0o644); err != nil {
		return Result{}, fmt.Errorf("write VM config: %w", err)
	}

	if err := ensureVMRunning(ctx, cfg, p, o.Progress); err != nil {
		return Result{}, err
	}

	sshClient, err := waitForSSH(ctx, cfg, p, signer, o.Progress)
	if err != nil {
		return Result{}, err
	}
	defer sshClient.Close()

	tsState, initRes, err := provisionVM(ctx, sshClient, cfg, linuxBin, o.TailscaleAuthKey, o.NoStart, o.Progress)
	if err != nil {
		return Result{}, err
	}

	switch {
	case o.NoStart:
		if err := disableSupervisor(ctx, cfg, p); err != nil {
			return Result{}, err
		}
		if err := stopVM(ctx, cfg, p, o.Progress); err != nil {
			return Result{}, err
		}
	case cfg.Autostart:
		progressf(o.Progress, "starting the VM's supervisor, which powers it off after %s without work and on when a run needs it", sleepAfter)
		if err := startSupervisor(ctx, p); err != nil {
			return Result{}, err
		}
	default:
		if err := disableSupervisor(ctx, cfg, p); err != nil {
			return Result{}, err
		}
	}

	pid, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		return Result{}, err
	}
	running := procState == vmProcRunning
	paused := procState == vmProcPaused
	disks := collectVMDisks(cfg, p)
	return Result{
		Platform:   "darwin-qemu",
		Dir:        baseDir,
		StateDir:   cfg.StateDir,
		Controller: controller,
		User:       username,
		Node:       initRes.Node,
		ClusterID:  initRes.ClusterID,
		Storage:    "zfs-qcow2",
		Running:    running && !o.NoStart,
		Service:    cmp.Or(initRes.Service, "hotbed-worker.service"),
		VM: &VMStatus{
			Hostname:   cfg.Hostname,
			Arch:       cfg.Arch,
			Ubuntu:     cfg.UbuntuVersion,
			CPUs:       cfg.CPUs,
			MemoryMB:   cfg.MemoryBytes / (1 << 20),
			SSHPort:    cfg.SSHPort,
			PID:        pid,
			Running:    running,
			Paused:     paused,
			QMPStatus:  qmp.QMPStatus,
			IOErrors:   qmp.IOErrors,
			Tailscale:  tsState,
			Autostart:  cfg.Autostart,
			Supervised: supervised(ctx, p),
			ConsoleLog: p.ConsoleLog,
		},
		Disks: disks,
		Next: []string{
			"hotbed worker status",
			"hotbed worker ssh",
			"hotbed worker trim",
			"hotbed nodes",
		},
	}, nil
}

// refuseResizeWhileRunning fails when the VM runs (or is paused) with other
// vCPUs or RAM than cfg asks for: QEMU keeps what it started with, so
// recording the new size would only claim it.
func refuseResizeWhileRunning(ctx context.Context, prev, cfg VMConfig, p VMPaths) error {
	if prev.CPUs == cfg.CPUs && prev.MemoryBytes == cfg.MemoryBytes {
		return nil
	}
	_, procState, _, err := vmDetailedState(ctx, p)
	if err != nil {
		return err
	}
	if procState != vmProcRunning && procState != vmProcPaused {
		return nil
	}
	return api.Errorf(api.CodeConflict, "the worker VM runs with %d vCPUs and %s RAM; %d vCPUs and %s RAM need it started again",
		prev.CPUs, FormatBytes(prev.MemoryBytes), cfg.CPUs, FormatBytes(cfg.MemoryBytes)).
		WithSuggestion("run `hotbed worker stop` (which stops the commands running on it), then this `hotbed worker init` again")
}

// workerRecord is the file worker init records the VM's setup in, its
// controller among it.
func workerRecord(baseDir string) string { return NewVMPaths(baseDir).Config }

// loadVM is the worker VM configured in dir (default ~/.hotbed).
func loadVM(dir string) (baseDir string, cfg VMConfig, p VMPaths, err error) {
	if baseDir, err = ResolveDir(dir); err != nil {
		return "", cfg, p, err
	}
	p = NewVMPaths(baseDir)
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", cfg, p, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir).
				WithSuggestion("run `hotbed worker init CONTROLLER`")
		}
		return "", cfg, p, err
	}
	return baseDir, cfg, p, nil
}

// GetStatus reports the local macOS QEMU worker VM's state and sparse disk usage.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	baseDir, cfg, p, err := loadVM(dir)
	if err != nil {
		return Status{}, err
	}
	pid, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		return Status{}, err
	}
	running := procState == vmProcRunning
	paused := procState == vmProcPaused
	workerRunning := false
	tsState := ""
	if running {
		if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
			if cl, err := dialSSH(ctx, cfg, p, signer, 3*time.Second); err == nil {
				defer cl.Close()
				if out, err := sshRun(ctx, cl, "systemctl is-active hotbed-worker.service 2>/dev/null || true"); err == nil {
					workerRunning = strings.TrimSpace(out) == "active"
				}
				if out, err := sshRun(ctx, cl, "sudo tailscale status --json 2>/dev/null || true"); err == nil {
					tsState = parseTailscaleSummary(out)
				}
			}
		}
	}
	return Status{
		Platform:   "darwin-qemu",
		Dir:        baseDir,
		StateDir:   cfg.StateDir,
		Controller: cfg.Controller,
		User:       cfg.User,
		Storage:    "zfs-qcow2",
		Running:    running && workerRunning,
		Service:    "hotbed-worker.service",
		VM: &VMStatus{
			Hostname:   cfg.Hostname,
			Arch:       cfg.Arch,
			Ubuntu:     cfg.UbuntuVersion,
			CPUs:       cfg.CPUs,
			MemoryMB:   cfg.MemoryBytes / (1 << 20),
			SSHPort:    cfg.SSHPort,
			PID:        pid,
			Running:    running,
			Paused:     paused,
			QMPStatus:  qmp.QMPStatus,
			IOErrors:   qmp.IOErrors,
			Tailscale:  tsState,
			Autostart:  cfg.Autostart,
			Supervised: supervised(ctx, p),
			ConsoleLog: p.ConsoleLog,
		},
		Disks: collectVMDisks(cfg, p),
	}, nil
}

// Start boots the configured macOS QEMU worker VM if it is not already
// running, then, with autostart, its supervisor, which from then on powers
// it as the controller says: it stays up for a sleep-after from now. The
// supervisor is stopped first, so the two never start or stop it at once.
func Start(ctx context.Context, dir string) (Status, error) {
	baseDir, cfg, p, err := loadVM(dir)
	if err != nil {
		return Status{}, err
	}
	if err := stopSupervisor(ctx, cfg, p); err != nil {
		return Status{}, err
	}
	if err := ensureVMRunning(ctx, cfg, p, os.Stderr); err != nil {
		return Status{}, err
	}
	if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
		if cl, err := waitForSSH(ctx, cfg, p, signer, os.Stderr); err == nil {
			defer cl.Close()
			_, _ = sshRun(ctx, cl, "sudo systemctl start hotbed-worker.service")
		}
	}
	if cfg.Autostart {
		if err := startSupervisor(ctx, p); err != nil {
			return Status{}, err
		}
	}
	return GetStatus(ctx, baseDir)
}

// Stop stops the VM's supervisor and removes its LaunchAgent, so nothing
// starts the VM again until hotbed worker start, not even a login, and shuts
// the VM down gracefully so ZFS exports cleanly.
func Stop(ctx context.Context, dir string) (Status, error) {
	baseDir, cfg, p, err := loadVM(dir)
	if err != nil {
		return Status{}, err
	}
	if err := disableSupervisor(ctx, cfg, p); err != nil {
		return Status{}, err
	}
	if err := stopVM(ctx, cfg, p, os.Stderr); err != nil {
		return Status{}, err
	}
	return GetStatus(ctx, baseDir)
}

// Trim flushes ZFS async/deferred frees (so autotrim=on punches freed extents
// without touching unused metaslabs) and runs `fstrim -a` inside the VM to
// punch freed clusters out of the host's qcow2 sparse images on APFS.
func Trim(ctx context.Context, dir string) (TrimResult, error) {
	baseDir, cfg, p, err := loadVM(dir)
	if err != nil {
		return TrimResult{}, err
	}
	_, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		return TrimResult{}, err
	}
	if procState == vmProcPaused {
		return TrimResult{}, pausedVMConflictError(qmp, "before trimming")
	}
	if procState != vmProcRunning {
		return TrimResult{}, api.Errorf(api.CodeConflict, "worker VM is not running").
			WithSuggestion("start it with `hotbed worker start` before trimming")
	}
	signer, err := loadSSHSigner(p.KeyPriv)
	if err != nil {
		return TrimResult{}, err
	}
	cl, err := dialSSH(ctx, cfg, p, signer, 10*time.Second)
	if err != nil {
		return TrimResult{}, fmt.Errorf("connect to worker VM over SSH: %w", err)
	}
	defer cl.Close()

	before := totalAllocated(collectVMDisks(cfg, p))
	// Flush async_destroy and advance past ZFS's 2-TXG deferred-free window so
	// autotrim=on punches out all freed blocks without touching unused metaslabs,
	// then run fstrim -a for the ext4 root qcow2.
	trimCmd := fmt.Sprintf("sudo zpool wait -t free %[1]s && for i in 1 2 3 4; do sudo zpool sync %[1]s; done && sudo fstrim -a && sleep 2", DefaultPoolName)
	if _, err := sshRun(ctx, cl, trimCmd); err != nil {
		return TrimResult{}, fmt.Errorf("trim inside VM: %w", err)
	}
	afterDisks := collectVMDisks(cfg, p)
	after := totalAllocated(afterDisks)
	return TrimResult{
		Dir:            baseDir,
		BeforeBytes:    before,
		AfterBytes:     after,
		ReclaimedBytes: max(0, before-after),
		Disks:          afterDisks,
	}, nil
}

// Destroy stops the VM, removes its LaunchAgent, and deletes ~/.hotbed/vm
// (plus ~/.hotbed/cache when o.PurgeCache is true).
func Destroy(ctx context.Context, o DestroyOptions) (DestroyResult, error) {
	baseDir, err := ResolveDir(o.Dir)
	if err != nil {
		return DestroyResult{}, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return DestroyResult{}, err
	}
	if err := stopSupervisor(ctx, cfg, p); err != nil {
		return DestroyResult{}, err
	}
	if err := stopVM(ctx, cfg, p, o.Progress); err != nil {
		return DestroyResult{}, fmt.Errorf("stop worker VM: %w", err)
	}
	var removed []string
	if plist := launchAgentPath(baseDir); plist != "" {
		if err := os.Remove(plist); err == nil {
			removed = append(removed, plist)
		}
	}
	if qmp := qmpSocketPath(p); qmp != p.QMPSock {
		_ = os.Remove(qmp)
	}
	if _, err := os.Stat(p.VMDir); err == nil {
		if err := os.RemoveAll(p.VMDir); err != nil {
			return DestroyResult{}, err
		}
		removed = append(removed, p.VMDir)
	}
	if o.PurgeCache {
		if _, err := os.Stat(p.CacheDir); err == nil {
			if err := os.RemoveAll(p.CacheDir); err != nil {
				return DestroyResult{}, err
			}
			removed = append(removed, p.CacheDir)
		}
	}
	if len(removed) == 0 {
		return DestroyResult{}, api.Errorf(api.CodeNotInitialized, "no worker VM found in %s", baseDir)
	}
	return DestroyResult{Dir: baseDir, Removed: removed}, nil
}

// SSH opens an interactive SSH session (or runs args) inside the macOS QEMU VM.
func SSH(ctx context.Context, dir string, args []string) (int, error) {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return 1, err
	}
	_, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		return 1, err
	}
	if procState == vmProcPaused {
		return 1, pausedVMConflictError(qmp, "")
	}
	if procState != vmProcRunning {
		return 1, api.Errorf(api.CodeConflict, "worker VM is not running").
			WithSuggestion("start it with `hotbed worker start`")
	}
	knownHosts, err := ensureVMKnownHosts(ctx, p, cfg.SSHPort)
	if err != nil {
		return 1, err
	}
	sshArgs := []string{
		"-i", p.KeyPriv,
		"-p", strconv.Itoa(cfg.SSHPort),
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "LogLevel=ERROR",
		cfg.User + "@127.0.0.1",
	}
	sshArgs = append(sshArgs, args...)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// --- QEMU / firmware / host resource helpers ---

func findQEMUBinaries(arch string) (qemuSys, qemuImg string, err error) {
	sysName := "qemu-system-aarch64"
	if arch == "amd64" {
		sysName = "qemu-system-x86_64"
	}
	qemuSys = findInPaths(sysName, "/opt/homebrew/bin", "/usr/local/bin")
	if qemuSys == "" {
		return "", "", api.Errorf(api.CodeInvalidArgs, "%s not found in PATH", sysName).
			WithSuggestion("install QEMU with `brew install qemu`")
	}
	qemuImg = findInPaths("qemu-img", filepath.Dir(qemuSys), "/opt/homebrew/bin", "/usr/local/bin")
	if qemuImg == "" {
		return "", "", api.Errorf(api.CodeInvalidArgs, "qemu-img not found in PATH").
			WithSuggestion("install QEMU with `brew install qemu`")
	}
	return qemuSys, qemuImg, nil
}

func findInPaths(name string, extraDirs ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range extraDirs {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func findUEFIFirmware(arch, qemuBin string) (code, varsTemplate string, err error) {
	shareDirs := []string{
		filepath.Clean(filepath.Join(filepath.Dir(qemuBin), "..", "share", "qemu")),
		"/opt/homebrew/share/qemu",
		"/usr/local/share/qemu",
		"/usr/share/qemu",
	}
	codeName := "edk2-aarch64-code.fd"
	varsName := ""
	if arch == "amd64" {
		codeName = "edk2-x86_64-code.fd"
		varsName = "edk2-i386-vars.fd"
	}
	for _, d := range shareDirs {
		p := filepath.Join(d, codeName)
		if _, err := os.Stat(p); err == nil {
			code = p
			if varsName != "" {
				vp := filepath.Join(d, varsName)
				if _, err := os.Stat(vp); err == nil {
					varsTemplate = vp
				}
			}
			break
		}
	}
	if code == "" {
		return "", "", api.Errorf(api.CodeInvalidArgs, "QEMU UEFI firmware %s not found", codeName).
			WithSuggestion("reinstall QEMU with `brew install qemu`")
	}
	if varsName != "" && varsTemplate == "" {
		return "", "", api.Errorf(api.CodeInvalidArgs, "QEMU UEFI vars template %s not found", varsName).
			WithSuggestion("reinstall QEMU with `brew install qemu`")
	}
	return code, varsTemplate, nil
}

func defaultVMCPUs() int {
	n := runtime.NumCPU() / 2
	if n < 2 {
		n = 2
	}
	return n
}

// defaultVMMemory is DefaultVMMemory for this Mac.
func defaultVMMemory() (int64, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("read this Mac's RAM (sysctl hw.memsize): %w", err)
	}
	return DefaultVMMemory(int64(total)), nil
}

func pickSSHPort() (int, error) {
	// Prefer a stable memorable localhost port when free.
	if l, err := net.Listen("tcp", "127.0.0.1:22222"); err == nil {
		l.Close()
		return 22222, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate SSH port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// --- Linux binary resolution & controller download ---

func resolveLinuxBinary(ctx context.Context, explicit, controller, cacheDir, arch string, hc *http.Client, progress io.Writer) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if err := verifyLinuxELF(abs, arch); err != nil {
			return "", api.Errorf(api.CodeInvalidArgs, "invalid --binary %s: %v", abs, err)
		}
		return abs, nil
	}

	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	hostPort, err := api.ControllerHostPort(controller)
	if err != nil {
		return "", err
	}
	cluster, _, err := api.Controller(hostPort, hc)
	if err != nil {
		return "", err
	}
	suggest := fmt.Sprintf("ensure the controller runs a CI build or pass --binary /path/to/hotbed-linux-%s", arch)
	b, err := cluster.GetBuild(ctx, &pb.GetBuildRequest{Os: "linux", Arch: arch})
	if err != nil {
		ae := api.FromConnect(err)
		if ae.Code == api.CodeNotFound {
			return "", api.Errorf(api.CodeNotFound, "controller %s has no published Linux %s build (%s)", hostPort, arch, ae.Message).
				WithSuggestion(fmt.Sprintf("pass --binary /path/to/hotbed-linux-%s or run the controller on a published CI build", arch))
		}
		return "", api.Errorf(ae.Code, "fetch Linux %s binary from controller %s: %s", arch, hostPort, ae.Message).
			WithSuggestion(suggest)
	}
	if b.Sha256 == "" || (b.DownloadUrl == "" && (b.Path == "" || !strings.HasPrefix(b.Path, "/builds/"))) {
		return "", api.Errorf(api.CodeInternal, "controller %s returned incomplete build metadata for linux/%s", hostPort, arch).
			WithSuggestion(suggest)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	outPath := filepath.Join(cacheDir, "hotbed-linux-"+arch)
	if sum, err := fileSHA256(outPath); err == nil && sum == b.Sha256 && verifyLinuxELF(outPath, arch) == nil {
		progressf(progress, "using cached Linux %s hotbed binary %s (build %d)", arch, outPath, b.Number)
		return outPath, nil
	}

	downloadURL := b.DownloadUrl
	if downloadURL == "" {
		base, err := api.ControllerBaseURL(controller)
		if err != nil {
			return "", err
		}
		downloadURL = base + b.Path
	}
	progressf(progress, "downloading Linux %s hotbed binary (build %d) from %s", arch, b.Number, downloadURL)
	dlCtx, dlCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer dlCancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := streamingHTTPClient(hc).Do(req)
	if err != nil {
		return "", api.Errorf(api.CodeControllerUnreachable, "download Linux %s binary from %s: %v", arch, downloadURL, err).
			WithSuggestion(suggest)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", api.Errorf(api.CodeNotFound, "download Linux %s binary from %s: %s", arch, downloadURL, resp.Status).
			WithSuggestion(suggest)
	}

	tmp, err := os.CreateTemp(cacheDir, ".hotbed-linux-"+arch+".*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download Linux %s binary: %w", arch, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != b.Sha256 {
		return "", fmt.Errorf("Linux %s binary from %s has sha256 %s, want %s", arch, downloadURL, got, b.Sha256)
	}
	if err := verifyLinuxELF(tmpPath, arch); err != nil {
		return "", fmt.Errorf("downloaded Linux %s binary from %s is invalid: %w", arch, downloadURL, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", err
	}
	return outPath, nil
}

// streamingHTTPClient clones base with Timeout disabled so large binary body
// reads rely on the request context and transport header timeouts instead of a
// short total-request Client.Timeout.
func streamingHTTPClient(base *http.Client) *http.Client {
	var c http.Client
	if base != nil {
		c = *base
	}
	c.Timeout = 0
	if c.Transport == nil {
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			t := dt.Clone()
			if t.ResponseHeaderTimeout == 0 {
				t.ResponseHeaderTimeout = 60 * time.Second
			}
			c.Transport = t
		}
	}
	return &c
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyLinuxELF(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("not a Linux ELF executable: %w", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("ELF class %s is not 64-bit", f.Class)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %s is not an executable", f.Type)
	}
	var want elf.Machine
	switch arch {
	case "amd64":
		want = elf.EM_X86_64
	case "arm64":
		want = elf.EM_AARCH64
	default:
		return fmt.Errorf("unsupported guest arch %q", arch)
	}
	if f.Machine != want {
		return fmt.Errorf("ELF machine %s does not match guest arch %s (want %s)", f.Machine, arch, want)
	}
	return nil
}

// --- Cloud image download, SSH keys, seed ISO, and sparse qcow2 disks ---

func ensureCloudImage(ctx context.Context, cacheDir, imageURL, filename, sumsURL string, progress io.Writer) (string, error) {
	dst := filepath.Join(cacheDir, filename)
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		progressf(progress, "using cached cloud image %s (%s)", dst, FormatBytes(fi.Size()))
		return dst, nil
	}

	var wantSHA string
	if sumsURL != "" {
		progressf(progress, "fetching checksums from %s", sumsURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch SHA256SUMS: %w", err)
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return "", rerr
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("fetch %s: HTTP %d", sumsURL, resp.StatusCode)
		}
		if sum, ok := ParseSHA256Sums(string(body), filename); ok {
			wantSHA = sum
		}
	}

	progressf(progress, "downloading %s", imageURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download cloud image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", imageURL, resp.StatusCode)
	}

	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer os.Remove(part)

	h := sha256.New()
	var written int64
	total := resp.ContentLength
	lastReport := time.Now()
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return "", werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if time.Since(lastReport) >= 5*time.Second {
				if total > 0 {
					progressf(progress, "downloaded %s / %s (%.0f%%)", FormatBytes(written), FormatBytes(total), 100*float64(written)/float64(total))
				} else {
					progressf(progress, "downloaded %s", FormatBytes(written))
				}
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return "", rerr
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	gotSHA := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && gotSHA != wantSHA {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", filename, gotSHA, wantSHA)
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	progressf(progress, "verified %s (%s)", filename, FormatBytes(written))
	return dst, nil
}

func ensureSSHKeypair(privPath, pubPath string) (ssh.Signer, string, error) {
	if _, err := os.Stat(privPath); err == nil {
		signer, err := loadSSHSigner(privPath)
		if err != nil {
			return nil, "", err
		}
		pubBytes, err := os.ReadFile(pubPath)
		if err != nil {
			pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			_ = os.WriteFile(pubPath, []byte(pub+"\n"), 0o644)
			return signer, pub, nil
		}
		return signer, strings.TrimSpace(string(pubBytes)), nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hotbed-vm")
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	authKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err := os.WriteFile(pubPath, []byte(authKey+"\n"), 0o644); err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	return signer, authKey, nil
}

func loadSSHSigner(privPath string) (ssh.Signer, error) {
	b, err := os.ReadFile(privPath)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func hostPublicKeys() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".ssh", "id_*.pub"))
	var out []string
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil {
			if line := strings.TrimSpace(string(b)); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// ensurePrivateVMDir creates vmDir with owner-only permissions (0700) and
// tightens an existing directory to 0700 so VM private keys, seed ISOs, and
// disk images are never readable by other local users.
func ensurePrivateVMDir(vmDir string) error {
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return err
	}
	return os.Chmod(vmDir, 0o700)
}

// ensureVMPrivatePermissions enforces 0700 on p.VMDir and 0600 on p.SeedISO
// and p.RootDisk when present (both hold guest SSH private host keys).
func ensureVMPrivatePermissions(p VMPaths) error {
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		return err
	}
	for _, path := range []string{p.SeedISO, p.RootDisk} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// prepareVMHostKeyForSetup returns the SSH host keypair to seed into cloud-init
// during Setup. A new keypair is generated and pinned only when creating a new
// VM disk (!diskExists). For an existing VM disk, any existing seeded keypair
// or pinned public key is preserved, and if neither exists no random keypair is
// generated so TOFU pins the guest's actual host key on first connect.
func prepareVMHostKeyForSetup(p VMPaths) (VMHostKey, error) {
	_, err := os.Stat(p.RootDisk)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return VMHostKey{}, err
	}
	diskExists := err == nil
	if !diskExists {
		hostKey, _, err := ensureVMHostKeypair(p.HostKeyPriv, p.HostKeyPub)
		return hostKey, err
	}
	if _, err := os.Stat(p.HostKeyPriv); err == nil {
		hostKey, _, err := ensureVMHostKeypair(p.HostKeyPriv, p.HostKeyPub)
		return hostKey, err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return VMHostKey{}, err
	}
	if pubKey, err := loadSSHPublicKey(p.HostKeyPub); err == nil {
		return VMHostKey{Public: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey)))}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return VMHostKey{}, err
	}
	return VMHostKey{}, nil
}

func ensureVMHostKeypair(privPath, pubPath string) (VMHostKey, ssh.PublicKey, error) {
	if err := ensurePrivateVMDir(filepath.Dir(privPath)); err != nil {
		return VMHostKey{}, nil, err
	}
	if privBytes, err := os.ReadFile(privPath); err == nil {
		signer, err := ssh.ParsePrivateKey(privBytes)
		if err != nil {
			return VMHostKey{}, nil, fmt.Errorf("parse VM SSH host key %s: %w", privPath, err)
		}
		pubKey := signer.PublicKey()
		pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey)))
		if _, err := os.Stat(pubPath); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(pubPath, []byte(pubLine+"\n"), 0o600); err != nil {
				return VMHostKey{}, nil, err
			}
		}
		return VMHostKey{PrivatePEM: string(privBytes), Public: pubLine}, pubKey, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return VMHostKey{}, nil, err
	}

	// Pre-existing VM already pinned via TOFU without a host-generated private key:
	// preserve the pinned public key rather than clobbering it.
	if pubKey, err := loadSSHPublicKey(pubPath); err == nil {
		pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey)))
		return VMHostKey{Public: pubLine}, pubKey, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return VMHostKey{}, nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return VMHostKey{}, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hotbed-vm-host")
	if err != nil {
		return VMHostKey{}, nil, err
	}
	privPEM := pem.EncodeToMemory(block)
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return VMHostKey{}, nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return VMHostKey{}, nil, err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err := os.WriteFile(pubPath, []byte(pubLine+"\n"), 0o600); err != nil {
		return VMHostKey{}, nil, err
	}
	return VMHostKey{PrivatePEM: string(privPEM), Public: pubLine}, sshPub, nil
}

func loadSSHPublicKey(pubPath string) (ssh.PublicKey, error) {
	b, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, err
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse SSH public key %s: %w", pubPath, err)
	}
	return pubKey, nil
}

func pinVMHostKeyAtomic(pubPath string, key ssh.PublicKey) error {
	dir := filepath.Dir(pubPath)
	if err := ensurePrivateVMDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(pubPath)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
	if _, err := tmp.WriteString(line); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), pubPath)
}

// hostKeyAlgorithmsFor returns the SSH host-key signature algorithms to offer
// for a pinned public key. RSA public keys report Type() == "ssh-rsa", which
// modern OpenSSH servers disable in favor of rsa-sha2-512 and rsa-sha2-256.
func hostKeyAlgorithmsFor(pub ssh.PublicKey) []string {
	if pub == nil {
		return nil
	}
	if pub.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{pub.Type()}
}

// vmHostKeyCallback returns an ssh.HostKeyCallback backed by pubPath.
// If pubPath already exists, the VM must present that exact key via
// ssh.FixedHostKey. If pubPath does not exist yet (a VM created before
// host-key seeding), the first observed key is pinned atomically to pubPath
// (mode 0600) and enforced via ssh.FixedHostKey on every subsequent handshake.
func vmHostKeyCallback(pubPath string) (ssh.HostKeyCallback, []string, error) {
	if pinned, err := loadSSHPublicKey(pubPath); err == nil {
		return ssh.FixedHostKey(pinned), hostKeyAlgorithmsFor(pinned), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}

	var mu sync.Mutex
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		if pinned, err := loadSSHPublicKey(pubPath); err == nil {
			return ssh.FixedHostKey(pinned)(hostname, remote, key)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := pinVMHostKeyAtomic(pubPath, key); err != nil {
			if errors.Is(err, fs.ErrExist) {
				pinned, lerr := loadSSHPublicKey(pubPath)
				if lerr != nil {
					return lerr
				}
				return ssh.FixedHostKey(pinned)(hostname, remote, key)
			}
			return fmt.Errorf("pin VM SSH host key %s: %w", pubPath, err)
		}
		return nil
	}
	return cb, nil, nil
}

var errHostKeyProbed = errors.New("host key probed")

// probeAndPinVMHostKey performs a single SSH transport handshake to 127.0.0.1:port
// using vmHostKeyCallback so interactive SSH and Go dialSSH share the exact same
// atomic HostKeyPub pin when pubPath does not yet exist.
func probeAndPinVMHostKey(ctx context.Context, port int, pubPath string, timeout time.Duration) error {
	if _, err := loadSSHPublicKey(pubPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cb, algos, err := vmHostKeyCallback(pubPath)
	if err != nil {
		return err
	}
	var probed bool
	probeCB := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := cb(hostname, remote, key); err != nil {
			return err
		}
		probed = true
		return errHostKeyProbed
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var d net.Dialer
	d.Timeout = timeout
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("probe VM SSH host key on %s: %w", addr, err)
	}
	defer conn.Close()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	c, _, _, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:              "probe",
		HostKeyCallback:   probeCB,
		HostKeyAlgorithms: algos,
		Timeout:           timeout,
	})
	if c != nil {
		_ = c.Close()
	}
	if probed {
		if _, lerr := loadSSHPublicKey(pubPath); lerr != nil {
			return lerr
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe VM SSH host key on %s: %w", addr, err)
	}
	return fmt.Errorf("probe VM SSH host key on %s: handshake completed without host key", addr)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ensureVMKnownHosts guarantees p.HostKeyPub is pinned (performing a single
// atomic SSH host-key probe when absent) and writes a 0600 known_hosts file
// derived strictly from p.HostKeyPub for OpenSSH with StrictHostKeyChecking=yes.
func ensureVMKnownHosts(ctx context.Context, p VMPaths, port int) (string, error) {
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		return "", err
	}
	knownHosts := filepath.Join(p.VMDir, "known_hosts")
	pubKey, err := loadSSHPublicKey(p.HostKeyPub)
	if errors.Is(err, fs.ErrNotExist) {
		if perr := probeAndPinVMHostKey(ctx, port, p.HostKeyPub, 5*time.Second); perr != nil {
			return "", perr
		}
		pubKey, err = loadSSHPublicKey(p.HostKeyPub)
	}
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("[127.0.0.1]:%d %s\n", port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey))))
	if err := writeFileAtomic(knownHosts, []byte(line), 0o600); err != nil {
		return "", err
	}
	return knownHosts, nil
}

func ensureSeedISO(ctx context.Context, isoPath, hostname, username string, pubKeys []string, hostKeys ...VMHostKey) error {
	if err := ensurePrivateVMDir(filepath.Dir(isoPath)); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(isoPath), "seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	contentDir := filepath.Join(tmpDir, "content")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contentDir, "meta-data"), []byte(CloudInitMetaData(hostname)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contentDir, "user-data"), []byte(CloudInitUserData(hostname, username, pubKeys, hostKeys...)), 0o600); err != nil {
		return err
	}
	tmpISO := filepath.Join(tmpDir, "seed.iso")
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "makehybrid",
		"-iso", "-joliet", "-default-volume-name", "cidata",
		"-o", tmpISO, contentDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build cloud-init seed ISO: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := os.Chmod(tmpISO, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpISO, isoPath); err != nil {
		return err
	}
	return os.Chmod(isoPath, 0o600)
}

func ensureVMDisks(ctx context.Context, qemuImg, cachedImg, fwVarsTemplate string, cfg VMConfig, p VMPaths, progress io.Writer) error {
	if err := ensurePrivateVMDir(p.VMDir); err != nil {
		return err
	}
	if _, err := os.Stat(p.RootDisk); errors.Is(err, fs.ErrNotExist) {
		progressf(progress, "creating sparse root disk %s (%s virtual)", p.RootDisk, FormatBytes(cfg.RootSizeBytes))
		if err := copyFileAtomic(cachedImg, p.RootDisk, 0o600); err != nil {
			return fmt.Errorf("create root disk: %w", err)
		}
		if err := runQEMUImg(ctx, qemuImg, "resize", p.RootDisk, strconv.FormatInt(cfg.RootSizeBytes, 10)); err != nil {
			os.Remove(p.RootDisk)
			return err
		}
	}
	if err := os.Chmod(p.RootDisk, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(p.ZFSDisk); errors.Is(err, fs.ErrNotExist) {
		progressf(progress, "creating sparse ZFS qcow2 disk %s (%s virtual)", p.ZFSDisk, FormatBytes(cfg.ZFSSizeBytes))
		if err := runQEMUImg(ctx, qemuImg, "create", "-f", "qcow2",
			"-o", "preallocation=off,cluster_size=65536",
			p.ZFSDisk, strconv.FormatInt(cfg.ZFSSizeBytes, 10)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.SwapDisk); errors.Is(err, fs.ErrNotExist) {
		progressf(progress, "creating sparse swap qcow2 disk %s (%s virtual)", p.SwapDisk, FormatBytes(cfg.SwapSizeBytes))
		if err := runQEMUImg(ctx, qemuImg, "create", "-f", "qcow2",
			"-o", "preallocation=off,cluster_size=65536",
			p.SwapDisk, strconv.FormatInt(cfg.SwapSizeBytes, 10)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.EFIVars); errors.Is(err, fs.ErrNotExist) {
		if cfg.Arch == "arm64" {
			f, err := os.OpenFile(p.EFIVars, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// QEMU virt pflash1 expects an exact 64 MiB raw flash image.
			if err := f.Truncate(64 << 20); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			if err := copyFileAtomic(fwVarsTemplate, p.EFIVars, 0o644); err != nil {
				return fmt.Errorf("copy UEFI vars: %w", err)
			}
		}
	}
	return nil
}

func runQEMUImg(ctx context.Context, qemuImg string, args ...string) error {
	cmd := exec.CommandContext(ctx, qemuImg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// --- QEMU process lifecycle ---

// qmpSocketPath returns p.QMPSock unless it would exceed macOS's 104-byte
// Unix domain socket path limit, in which case it uses a short /tmp path keyed
// on the symlink-resolved VMDir so symlinked and canonical paths agree on the
// same socket.
func qmpSocketPath(p VMPaths) string {
	if len(p.QMPSock) < 100 {
		return p.QMPSock
	}
	key := filepath.Clean(p.VMDir)
	if resolved := evalExistingSymlinks(key); resolved != "" {
		key = resolved
	}
	return hashedQMPSocketPath(key)
}

func hashedQMPSocketPath(vmDir string) string {
	sum := sha256.Sum256([]byte(vmDir))
	return filepath.Join("/tmp", "hotbed-qmp-"+hex.EncodeToString(sum[:6])+".sock")
}

type vmProcState int

const (
	vmProcMissing vmProcState = iota
	vmProcRunning
	vmProcUnknown
	vmProcPaused
)

// runPS executes ps to inspect a single PID's stat and full command line.
// Overridable in tests.
var runPS = func(ctx context.Context, pid int) ([]byte, error) {
	psBin := findInPaths("ps", "/bin", "/usr/bin")
	if psBin == "" {
		psBin = "/bin/ps"
	}
	return exec.CommandContext(ctx, psBin, "-ww", "-p", strconv.Itoa(pid), "-o", "stat=,command=").Output()
}

// processCommandLine returns the full command line and state for pid:
//   - (cmd, vmProcRunning, nil) when pid is a live, non-zombie process
//   - ("", vmProcMissing, nil) when pid is confirmed absent or a zombie
//   - ("", vmProcUnknown, err) when inspection failed (ctx cancellation, timeout, exec error)
//
// Overridable in tests.
var processCommandLine = darwinProcessCommandLine

func darwinProcessCommandLine(ctx context.Context, pid int) (string, vmProcState, error) {
	if err := ctx.Err(); err != nil {
		return "", vmProcUnknown, fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	psCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := runPS(psCtx, pid)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", vmProcUnknown, fmt.Errorf("inspect pid %d: %w", pid, ctxErr)
		}
		if psErr := psCtx.Err(); psErr != nil {
			return "", vmProcUnknown, fmt.Errorf("inspect pid %d: ps timed out: %w", pid, psErr)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(string(out)) == "" {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return "", vmProcMissing, nil
			}
			return "", vmProcUnknown, fmt.Errorf("inspect pid %d: ps exited with status 1 while process is still present", pid)
		}
		return "", vmProcUnknown, fmt.Errorf("inspect pid %d with ps: %w", pid, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return "", vmProcMissing, nil
		}
		return "", vmProcUnknown, fmt.Errorf("inspect pid %d: ps returned empty output while process is still present", pid)
	}
	cmd, state := parsePSStatCommand(string(out))
	if state == vmProcUnknown {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return "", vmProcMissing, nil
		}
		return "", vmProcUnknown, fmt.Errorf("inspect pid %d: unexpected ps output %q", pid, strings.TrimSpace(string(out)))
	}
	return cmd, state, nil
}

func parsePSStatCommand(out string) (string, vmProcState) {
	line := strings.TrimSpace(out)
	if line == "" {
		return "", vmProcMissing
	}
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		if strings.HasPrefix(line, "Z") {
			return "", vmProcMissing
		}
		return "", vmProcUnknown
	}
	stat := line[:idx]
	if strings.HasPrefix(stat, "Z") {
		return "", vmProcMissing
	}
	cmd := strings.TrimSpace(line[idx:])
	if cmd == "" {
		return "", vmProcUnknown
	}
	return cmd, vmProcRunning
}

// isVMProcess reports whether pid is a live, non-zombie QEMU process belonging
// to this VM (matching qemu-system* and this VM's exact -pidfile or QMP socket).
func isVMProcess(ctx context.Context, pid int, p VMPaths) (vmProcState, error) {
	cmdLine, state, err := processCommandLine(ctx, pid)
	if state != vmProcRunning {
		return state, err
	}
	if isVMProcessCmd(cmdLine, p) {
		return vmProcRunning, nil
	}
	return vmProcMissing, nil
}

func isVMProcessCmd(cmdLine string, p VMPaths) bool {
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" {
		return false
	}
	// Inspect the executable before the first flag so paths with spaces still
	// match while wrappers or tools merely mentioning qemu-system (e.g.
	// `grep qemu-system ...`, `sh -c "qemu-system ..."`, or `env /path/qemu-system ...`)
	// are rejected.
	idx := indexFlagStart(cmdLine)
	if idx < 0 {
		return false
	}
	exe := strings.Trim(strings.TrimSpace(cmdLine[:idx]), `"'`)
	base := filepath.Base(exe)
	if !strings.HasPrefix(base, "qemu-system") || strings.ContainsAny(base, " \t") || strings.Contains(exe, " /") {
		return false
	}
	args := cmdLine[idx:] // begins with whitespace + '-'
	return vmCmdMatchesPaths(args, resolvePathCandidates(p.PIDFile), qmpSocketCandidates(p))
}

func qmpSocketCandidates(p VMPaths) []string {
	seen := make(map[string]struct{}, 6)
	var out []string
	add := func(path string) {
		for _, c := range resolvePathCandidates(path) {
			if _, ok := seen[c]; !ok {
				seen[c] = struct{}{}
				out = append(out, c)
			}
		}
	}
	add(qmpSocketPath(p))
	if len(p.QMPSock) >= 100 {
		// Also match VMs launched before qmpSocketPath resolved symlinks.
		add(hashedQMPSocketPath(filepath.Clean(p.VMDir)))
	}
	if realVMDir := evalExistingSymlinks(p.VMDir); realVMDir != "" && realVMDir != filepath.Clean(p.VMDir) {
		realP := p
		realP.VMDir = realVMDir
		realP.QMPSock = filepath.Join(realVMDir, filepath.Base(p.QMPSock))
		add(qmpSocketPath(realP))
	}
	return out
}

func indexFlagStart(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if isSpaceByte(s[i]) && s[i+1] == '-' {
			return i
		}
	}
	return -1
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t'
}

func isQuoteByte(b byte) bool {
	return b == '"' || b == '\''
}

// resolvePathCandidates returns cleaned path variants for target, including
// symlink-resolved forms (resolving the deepest existing ancestor when target
// does not yet exist on disk).
func resolvePathCandidates(target string) []string {
	if target == "" {
		return nil
	}
	clean := filepath.Clean(target)
	out := []string{clean}
	if resolved := evalExistingSymlinks(clean); resolved != "" && resolved != clean {
		out = append(out, resolved)
	}
	return out
}

func evalExistingSymlinks(path string) string {
	cur := filepath.Clean(path)
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			parts := make([]string, 0, 1+len(tail))
			parts = append(parts, resolved)
			parts = append(parts, tail...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func vmCmdMatchesPaths(args string, pidFiles, qmpSocks []string) bool {
	for _, pidFile := range pidFiles {
		if matchPIDFileArg(args, pidFile) {
			return true
		}
	}
	for _, qmpSock := range qmpSocks {
		if matchQMPSockArg(args, qmpSock) {
			return true
		}
	}
	return false
}

// matchPIDFileArg reports whether args contains a -pidfile / --pidfile flag
// whose value is exactly pidFile (unquoted or quoted, with argument boundaries).
func matchPIDFileArg(args, pidFile string) bool {
	if pidFile == "" {
		return false
	}
	for i := 0; i < len(args); i++ {
		if !isSpaceByte(args[i]) {
			continue
		}
		j := i + 1
		for j < len(args) && isSpaceByte(args[j]) {
			j++
		}
		rest := args[j:]
		var afterFlag string
		switch {
		case strings.HasPrefix(rest, "--pidfile"):
			afterFlag = rest[len("--pidfile"):]
		case strings.HasPrefix(rest, "-pidfile"):
			afterFlag = rest[len("-pidfile"):]
		default:
			continue
		}
		if len(afterFlag) == 0 {
			continue
		}
		if afterFlag[0] == '=' {
			afterFlag = afterFlag[1:]
		} else if isSpaceByte(afterFlag[0]) {
			afterFlag = strings.TrimLeft(afterFlag, " \t")
		} else {
			continue
		}
		if matchTokenValue(afterFlag, pidFile, false) {
			return true
		}
	}
	return false
}

// matchQMPSockArg reports whether args binds this VM's QMP socket via
// `-qmp unix:<qmpSock>[,...]` or `path=<qmpSock>[,...]` with exact boundaries.
func matchQMPSockArg(args, qmpSock string) bool {
	if qmpSock == "" {
		return false
	}
	for i := 0; i < len(args); i++ {
		if !isSpaceByte(args[i]) {
			continue
		}
		j := i + 1
		for j < len(args) && isSpaceByte(args[j]) {
			j++
		}
		rest := args[j:]
		var afterFlag string
		switch {
		case strings.HasPrefix(rest, "--qmp"):
			afterFlag = rest[len("--qmp"):]
		case strings.HasPrefix(rest, "-qmp"):
			afterFlag = rest[len("-qmp"):]
		default:
			continue
		}
		if len(afterFlag) == 0 {
			continue
		}
		if afterFlag[0] == '=' {
			afterFlag = afterFlag[1:]
		} else if isSpaceByte(afterFlag[0]) {
			afterFlag = strings.TrimLeft(afterFlag, " \t")
		} else {
			continue
		}
		if matchQMPUnixValue(afterFlag, qmpSock) {
			return true
		}
	}

	const pathKey = "path="
	for offset := 0; offset < len(args); {
		rel := strings.Index(args[offset:], pathKey)
		if rel < 0 {
			break
		}
		pos := offset + rel
		offset = pos + len(pathKey)
		if pos == 0 {
			continue
		}
		prev := args[pos-1]
		if !isSpaceByte(prev) && prev != ',' && prev != ':' && !isQuoteByte(prev) {
			continue
		}
		val := args[pos+len(pathKey):]
		if matchTokenValue(val, qmpSock, true) {
			return true
		}
		// Handle an outer quote around the whole option value, such as
		// `-chardev "socket,id=qmp,path=/path/to/qmp.sock"`.
		if strings.HasPrefix(val, qmpSock) {
			rem := val[len(qmpSock):]
			if len(rem) > 0 && isQuoteByte(rem[0]) && isArgDelimiter(rem[1:], false) && strings.Count(args[:pos], string(rem[0]))%2 == 1 {
				return true
			}
		}
	}
	return false
}

func matchQMPUnixValue(val, qmpSock string) bool {
	if val == "" || qmpSock == "" {
		return false
	}
	var outerQuote byte
	if isQuoteByte(val[0]) {
		outerQuote = val[0]
		val = val[1:]
	}
	if !strings.HasPrefix(val, "unix:") {
		return false
	}
	afterUnix := val[len("unix:"):]
	if outerQuote != 0 {
		if !strings.HasPrefix(afterUnix, qmpSock) {
			return false
		}
		rem := afterUnix[len(qmpSock):]
		if len(rem) > 0 && rem[0] == ',' {
			return true
		}
		return len(rem) > 0 && rem[0] == outerQuote && isArgDelimiter(rem[1:], false)
	}
	return matchTokenValue(afterUnix, qmpSock, true)
}

func matchTokenValue(s, want string, allowComma bool) bool {
	if s == "" || want == "" {
		return false
	}
	var quote byte
	if isQuoteByte(s[0]) {
		quote = s[0]
		s = s[1:]
	}
	if !strings.HasPrefix(s, want) {
		return false
	}
	rem := s[len(want):]
	if quote != 0 {
		if len(rem) == 0 || rem[0] != quote {
			return false
		}
		rem = rem[1:]
	}
	return isArgDelimiter(rem, allowComma)
}

func isArgDelimiter(rem string, allowComma bool) bool {
	if len(rem) == 0 {
		return true
	}
	if isSpaceByte(rem[0]) {
		return true
	}
	return allowComma && rem[0] == ','
}

func vmProcessAlive(ctx context.Context, pid int, p VMPaths) (vmProcState, error) {
	if pid <= 0 {
		return vmProcMissing, nil
	}
	if err := ctx.Err(); err != nil {
		return vmProcUnknown, fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return vmProcMissing, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return vmProcUnknown, fmt.Errorf("probe pid %d: %w", pid, err)
		}
	}
	return isVMProcess(ctx, pid, p)
}

const defaultMinHostFreeBytes int64 = 2 << 30 // 2 GiB

var (
	// minHostFreeBytes is the minimum free space required on the host
	// filesystem hosting p.VMDir before resuming a paused VM. Overridable in tests.
	minHostFreeBytes = defaultMinHostFreeBytes
	// hostFreeBytes reports available bytes on the filesystem containing dir.
	// Overridable in tests.
	hostFreeBytes = darwinHostFreeBytes
	// resumeVerifyTimeout and resumeVerifyInterval bound how long resumePausedVM
	// polls query-status after sending QMP cont. Overridable in tests.
	resumeVerifyTimeout  = 2 * time.Second
	resumeVerifyInterval = 100 * time.Millisecond
	// pausedVMCheckInterval is how often the supervisor polls for a paused VM.
	// Overridable in tests.
	pausedVMCheckInterval = 15 * time.Second
	// resyncGuestClock steps the guest clock over SSH after resuming a paused VM.
	// Overridable in tests.
	resyncGuestClock = darwinResyncGuestClock

	vmRecoverMu sync.Mutex
)

// vmQMPInfo captures the QEMU run state and any non-"ok" block device io-status
// entries reported over QMP.
type vmQMPInfo struct {
	Known     bool
	Running   bool
	QMPStatus string
	IOErrors  []string
}

// isPaused reports whether QEMU is in a paused state that can be resumed via
// QMP `cont` (or quit directly if stopping). Other non-running statuses such as
// "suspended", "debug", or "guest-panicked" are intentionally excluded so they
// do not trigger `qmpQuit` instead of clean ACPI/SSH poweroff or `cont` recovery.
func (q vmQMPInfo) isPaused() bool {
	return q.QMPStatus == "paused" || q.QMPStatus == "io-error"
}

func (q vmQMPInfo) isIOErrorPause() bool {
	return q.QMPStatus == "io-error" || len(q.IOErrors) > 0
}

func formatQMPPauseDetail(q vmQMPInfo) string {
	switch {
	case q.QMPStatus != "" && len(q.IOErrors) > 0:
		return fmt.Sprintf("%s: %s", q.QMPStatus, strings.Join(q.IOErrors, ", "))
	case q.QMPStatus != "":
		return q.QMPStatus
	case len(q.IOErrors) > 0:
		return strings.Join(q.IOErrors, ", ")
	default:
		return "paused"
	}
}

func pausedVMConflictError(q vmQMPInfo, suffix string) *api.Error {
	detail := formatQMPPauseDetail(q)
	suggestion := "resume it with `hotbed worker start`"
	if q.isIOErrorPause() {
		suggestion = "free disk space on the host and run `hotbed worker start`"
	}
	if suffix != "" {
		suggestion += " " + suffix
	}
	return api.Errorf(api.CodeConflict, "worker VM is paused (%s)", detail).WithSuggestion(suggestion)
}

func darwinHostFreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("statfs %s: invalid block size %d", dir, st.Bsize)
	}
	bsize := uint64(st.Bsize)
	if st.Bavail > math.MaxInt64/bsize {
		return math.MaxInt64, nil
	}
	return int64(st.Bavail * bsize), nil
}

// qmpInspectVM queries QMP `query-status` via QMPQueryStatus, and when the
// guest is paused or in an I/O error state, also queries `query-block` to
// collect any block devices whose `io-status` is not `"ok"` (such as
// `"swap: nospace"`).
func qmpInspectVM(ctx context.Context, sockPath string) (vmQMPInfo, error) {
	status, err := QMPQueryStatus(ctx, sockPath)
	if err != nil {
		return vmQMPInfo{}, err
	}
	info := vmQMPInfo{
		Known:     true,
		Running:   status == "running",
		QMPStatus: status,
	}
	if info.isPaused() {
		if c, derr := dialQMP(ctx, sockPath); derr == nil {
			defer c.Close()
			if blkRaw, berr := c.execute("query-block"); berr == nil {
				info.IOErrors = parseQMPBlockIOErrors(blkRaw)
			}
		}
	}
	return info, nil
}

func parseQMPBlockIOErrors(raw json.RawMessage) []string {
	var blocks []struct {
		Device   string `json:"device"`
		Qdev     string `json:"qdev"`
		IOStatus string `json:"io-status"`
		Inserted *struct {
			NodeName string `json:"node-name"`
			File     string `json:"file"`
		} `json:"inserted"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var errs []string
	for _, b := range blocks {
		if b.IOStatus == "" || b.IOStatus == "ok" {
			continue
		}
		name := b.Device
		if name == "" && b.Inserted != nil {
			name = cmp.Or(b.Inserted.NodeName, filepath.Base(b.Inserted.File))
		}
		if name == "" {
			name = cmp.Or(b.Qdev, "block")
		}
		errs = append(errs, fmt.Sprintf("%s: %s", name, b.IOStatus))
	}
	return errs
}

func qmpCont(ctx context.Context, sockPath string) error {
	c, err := dialQMP(ctx, sockPath)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.execute("cont")
	return err
}

func qmpQuit(ctx context.Context, sockPath string) error {
	c, err := dialQMP(ctx, sockPath)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.execute("quit")
	return err
}

func vmDetailedState(ctx context.Context, p VMPaths) (int, vmProcState, vmQMPInfo, error) {
	if err := ctx.Err(); err != nil {
		return 0, vmProcUnknown, vmQMPInfo{}, err
	}
	b, err := os.ReadFile(p.PIDFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, vmProcMissing, vmQMPInfo{}, nil
		}
		return 0, vmProcUnknown, vmQMPInfo{}, fmt.Errorf("read %s: %w", p.PIDFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, vmProcMissing, vmQMPInfo{}, nil
	}
	state, err := vmProcessAlive(ctx, pid, p)
	if state != vmProcRunning {
		return 0, state, vmQMPInfo{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	qmp, qerr := qmpInspectVM(qctx, qmpSocketPath(p))
	cancel()
	if qerr == nil && qmp.isPaused() {
		return pid, vmProcPaused, qmp, nil
	}
	return pid, vmProcRunning, qmp, nil
}

func vmProcessState(ctx context.Context, p VMPaths) (int, vmProcState, error) {
	pid, state, _, err := vmDetailedState(ctx, p)
	return pid, state, err
}

// resumePausedVM resumes a live but paused QEMU VM via QMP `cont` once the
// host filesystem hosting p.VMDir has at least minHostFreeBytes available,
// verifies QEMU reports `running: true`, and best-effort resyncs the guest
// clock over SSH.
func resumePausedVM(ctx context.Context, cfg VMConfig, p VMPaths, qmp vmQMPInfo, progress io.Writer) error {
	detail := formatQMPPauseDetail(qmp)
	freeDir := p.VMDir
	freeBytes, err := hostFreeBytes(freeDir)
	if err != nil {
		return fmt.Errorf("worker VM is paused (%s) and checking host free space on %s failed: %w", detail, freeDir, err)
	}
	if freeBytes < minHostFreeBytes {
		return api.Errorf(api.CodeConflict,
			"worker VM is paused (%s) and host filesystem %s has only %s free (need at least %s to resume)",
			detail, freeDir, FormatBytes(freeBytes), FormatBytes(minHostFreeBytes),
		).WithSuggestion(fmt.Sprintf("free disk space on the host (at least %s free) and run `hotbed worker start`", FormatBytes(minHostFreeBytes)))
	}

	progressf(progress, "resuming paused worker VM (%s; host free space %s)", detail, FormatBytes(freeBytes))
	qmpSock := qmpSocketPath(p)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = qmpCont(cctx, qmpSock)
	cancel()
	if err != nil {
		return fmt.Errorf("resume paused worker VM via QMP cont: %w", err)
	}

	deadline := time.Now().Add(resumeVerifyTimeout)
	var latest vmQMPInfo
	for {
		vctx, vcancel := context.WithTimeout(ctx, 2*time.Second)
		st, verr := qmpInspectVM(vctx, qmpSock)
		vcancel()
		if verr != nil {
			return fmt.Errorf("verify resumed worker VM status: %w", verr)
		}
		latest = st
		if st.Running {
			break
		}
		if time.Now().After(deadline) {
			return api.Errorf(api.CodeConflict,
				"worker VM remained paused after QMP cont (%s; host free space %s)",
				formatQMPPauseDetail(latest), FormatBytes(freeBytes),
			).WithSuggestion("check host disk space and " + p.QEMULog + ", then retry `hotbed worker start`")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(resumeVerifyInterval):
		}
	}

	if err := resyncGuestClock(ctx, cfg, p); err != nil {
		progressf(progress, "resumed worker VM, but guest clock resync over SSH failed (best effort): %v", err)
	}
	return nil
}

func darwinResyncGuestClock(ctx context.Context, cfg VMConfig, p VMPaths) error {
	if cfg.SSHPort <= 0 || cfg.User == "" {
		return nil
	}
	signer, err := loadSSHSigner(p.KeyPriv)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cl, err := dialSSH(rctx, cfg, p, signer, 5*time.Second)
	if err != nil {
		return err
	}
	defer cl.Close()
	nowUTC := time.Now().UTC().Format("2006-01-02 15:04:05")
	cmd := fmt.Sprintf(
		"sudo date -u -s %s >/dev/null 2>&1 || true; "+
			"if command -v chronyc >/dev/null 2>&1; then sudo chronyc -a makestep >/dev/null 2>&1 || true; fi; "+
			"if systemctl is-active --quiet systemd-timesyncd 2>/dev/null; then sudo systemctl restart systemd-timesyncd >/dev/null 2>&1 || true; fi",
		shell.Quote(nowUTC),
	)
	_, err = sshRun(rctx, cl, cmd)
	return err
}

// checkAndRecoverPausedVM inspects the worker VM once and, if QEMU has paused
// it due to an I/O error (such as host ENOSPC), logs the QMP io-status and host
// free space and resumes it via QMP `cont` once host free space meets
// minHostFreeBytes.
func checkAndRecoverPausedVM(ctx context.Context, cfg VMConfig, p VMPaths, log *slog.Logger) (bool, error) {
	vmRecoverMu.Lock()
	defer vmRecoverMu.Unlock()

	if log == nil {
		log = slog.Default()
	}
	pid, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil || procState != vmProcPaused {
		return false, err
	}
	if !qmp.isIOErrorPause() {
		return false, nil
	}
	freeBytes, freeErr := hostFreeBytes(p.VMDir)
	if freeErr != nil {
		log.Warn("supervise: worker VM is paused; failed to check host disk free space",
			"node", cfg.Hostname, "pid", pid, "qmpStatus", qmp.QMPStatus, "ioErrors", qmp.IOErrors, "err", freeErr)
		return false, freeErr
	}
	if freeBytes < minHostFreeBytes {
		log.Warn("supervise: worker VM is paused; waiting for host disk space before resuming",
			"node", cfg.Hostname, "pid", pid, "qmpStatus", qmp.QMPStatus, "ioErrors", qmp.IOErrors,
			"hostFree", FormatBytes(freeBytes), "minFree", FormatBytes(minHostFreeBytes))
		return false, nil
	}
	log.Warn("supervise: worker VM is paused; host disk space recovered, resuming via QMP cont",
		"node", cfg.Hostname, "pid", pid, "qmpStatus", qmp.QMPStatus, "ioErrors", qmp.IOErrors,
		"hostFree", FormatBytes(freeBytes), "minFree", FormatBytes(minHostFreeBytes))
	if err := resumePausedVM(ctx, cfg, p, qmp, nil); err != nil {
		log.Warn("supervise: failed to resume paused worker VM",
			"node", cfg.Hostname, "pid", pid, "qmpStatus", qmp.QMPStatus, "ioErrors", qmp.IOErrors,
			"hostFree", FormatBytes(freeBytes), "err", err)
		return false, err
	}
	log.Info("supervise: resumed paused worker VM",
		"node", cfg.Hostname, "pid", pid, "qmpStatus", qmp.QMPStatus, "ioErrors", qmp.IOErrors,
		"hostFree", FormatBytes(freeBytes))
	return true, nil
}

func darwinWatchPausedVM(ctx context.Context, dir string, log *slog.Logger) {
	_, cfg, p, err := loadVM(dir)
	if err != nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	_, _ = checkAndRecoverPausedVM(ctx, cfg, p, log)
	t := time.NewTicker(pausedVMCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = checkAndRecoverPausedVM(ctx, cfg, p, log)
		}
	}
}

func ensureVMRunning(ctx context.Context, cfg VMConfig, p VMPaths, progress io.Writer) error {
	pid, procState, qmp, err := vmDetailedState(ctx, p)
	if err != nil {
		return err
	}
	switch procState {
	case vmProcRunning:
		progressf(progress, "worker VM is already running (pid %d)", pid)
		return nil
	case vmProcPaused:
		return resumePausedVM(ctx, cfg, p, qmp, progress)
	}
	qmpSock := qmpSocketPath(p)
	_ = os.Remove(qmpSock)
	_ = os.Remove(p.PIDFile)

	args, err := QEMUArgs(cfg, p, qmpSock)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.QEMULog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	progressf(progress, "starting QEMU VM (%s, %d vCPUs, %s RAM, ssh 127.0.0.1:%d)",
		cfg.Hostname, cfg.CPUs, FormatBytes(cfg.MemoryBytes), cfg.SSHPort)
	cmd := exec.Command(cfg.QEMUBinary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start QEMU: %w", err)
	}
	logFile.Close()
	// Reap in background if it exits while we're still running; if it lives past
	// this CLI invocation, init adopts it cleanly.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			logTail, _ := os.ReadFile(p.QEMULog)
			return fmt.Errorf("QEMU exited immediately (%v): %s", err, strings.TrimSpace(string(logTail)))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		qctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, qerr := QMPQueryStatus(qctx, qmpSock)
		cancel()
		if qerr == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for QEMU QMP socket at %s (check %s)", qmpSock, p.QEMULog)
}

func stopVM(ctx context.Context, cfg VMConfig, p VMPaths, progress io.Writer) error {
	pid, procState, err := vmProcessState(ctx, p)
	switch procState {
	case vmProcUnknown:
		return err
	case vmProcMissing:
		_ = os.Remove(p.PIDFile)
		_ = os.Remove(qmpSocketPath(p))
		return nil
	}
	progressf(progress, "shutting down worker VM (pid %d)", pid)

	// Prefer clean guest poweroff over SSH so ZFS exports cleanly; fall back to QMP ACPI powerdown.
	// If the VM is paused (e.g. io-error), the guest cannot process SSH or ACPI, so quit QEMU cleanly via QMP.
	poweredDown := false
	if procState == vmProcRunning && cfg.SSHPort > 0 && cfg.User != "" {
		if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
			if cl, err := dialSSH(ctx, cfg, p, signer, 3*time.Second); err == nil {
				_, _ = sshRun(ctx, cl, "sudo systemctl poweroff || sudo poweroff")
				cl.Close()
				poweredDown = true
			}
		}
	}
	if !poweredDown {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if procState == vmProcPaused {
			_ = qmpQuit(qctx, qmpSocketPath(p))
		} else {
			_ = QMPPowerdown(qctx, qmpSocketPath(p))
		}
		cancel()
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, err := vmProcessAlive(ctx, pid, p)
		switch state {
		case vmProcMissing:
			_ = os.Remove(p.PIDFile)
			_ = os.Remove(qmpSocketPath(p))
			return nil
		case vmProcUnknown:
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	state, err := vmProcessAlive(ctx, pid, p)
	if state == vmProcUnknown {
		return err
	}
	if state == vmProcRunning {
		if kerr := syscall.Kill(pid, syscall.SIGTERM); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			return fmt.Errorf("send SIGTERM to worker VM (pid %d): %w", pid, kerr)
		}
		termDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(termDeadline) {
			state, err = vmProcessAlive(ctx, pid, p)
			if state == vmProcMissing {
				break
			}
			if state == vmProcUnknown {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if state == vmProcRunning {
		state, err = vmProcessAlive(ctx, pid, p)
		if state == vmProcUnknown {
			return err
		}
	}
	if state == vmProcRunning {
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			return fmt.Errorf("send SIGKILL to worker VM (pid %d): %w", pid, kerr)
		}
		killDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(killDeadline) {
			state, err = vmProcessAlive(ctx, pid, p)
			if state == vmProcMissing {
				break
			}
			if state == vmProcUnknown {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if state != vmProcMissing {
		return fmt.Errorf("worker VM (pid %d) did not exit after SIGKILL", pid)
	}
	_ = os.Remove(p.PIDFile)
	_ = os.Remove(qmpSocketPath(p))
	return nil
}

// --- SSH provisioning inside the Ubuntu guest ---

func dialSSH(ctx context.Context, cfg VMConfig, p VMPaths, signer ssh.Signer, timeout time.Duration) (*ssh.Client, error) {
	hostKeyCB, hostKeyAlgos, err := vmHostKeyCallback(p.HostKeyPub)
	if err != nil {
		return nil, err
	}
	sshCfg := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           timeout,
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.SSHPort))
	var d net.Dialer
	d.Timeout = timeout
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func waitForSSH(ctx context.Context, cfg VMConfig, p VMPaths, signer ssh.Signer, progress io.Writer) (*ssh.Client, error) {
	progressf(progress, "waiting for Ubuntu VM to boot and accept SSH on 127.0.0.1:%d", cfg.SSHPort)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_, procState, qmp, err := vmDetailedState(ctx, p)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else if procState == vmProcMissing {
			logTail, _ := os.ReadFile(p.QEMULog)
			return nil, fmt.Errorf("worker VM exited while booting (see %s): %s", p.ConsoleLog, strings.TrimSpace(string(logTail)))
		} else if procState == vmProcPaused {
			return nil, pausedVMConflictError(qmp, "")
		}
		if cl, err := dialSSH(ctx, cfg, p, signer, 3*time.Second); err == nil {
			return cl, nil
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for SSH in worker VM (check %s)", p.ConsoleLog)
}

func sshRun(ctx context.Context, cl *ssh.Client, command string) (string, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			return stdout.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.String(), nil
	}
}

func sshUploadFile(ctx context.Context, cl *ssh.Client, localPath, remotePath string, mode string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = f
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	tmp := remotePath + ".new"
	cmd := fmt.Sprintf("sudo tee %s >/dev/null && sudo chmod %s %s && sudo mv %s %s",
		shell.Quote(tmp), mode, shell.Quote(tmp), shell.Quote(tmp), shell.Quote(remotePath))
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("upload %s to %s: %w: %s", localPath, remotePath, err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

func provisionVM(ctx context.Context, cl *ssh.Client, cfg VMConfig, linuxBin, tsAuthKey string, noStart bool, progress io.Writer) (tsState string, initRes Result, err error) {
	progressf(progress, "waiting for cloud-init inside VM")
	if _, err := sshRun(ctx, cl, "sudo cloud-init status --wait >/dev/null 2>&1 || true"); err != nil {
		return "", Result{}, err
	}

	progressf(progress, "installing ZFS, git, and Tailscale inside Ubuntu VM")
	pkgScript := `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v zpool >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq zfsutils-linux git curl ca-certificates
fi
if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sudo sh
fi
sudo systemctl enable --now tailscaled
`
	if _, err := sshRun(ctx, cl, pkgScript); err != nil {
		return "", Result{}, fmt.Errorf("install packages in VM: %w", err)
	}

	progressf(progress, "configuring sparse discard swap disk inside VM")
	swapScript := fmt.Sprintf(`set -euo pipefail
if ! swapon --show=NAME --noheadings | grep -q .; then
  if [ "$(sudo blkid -s TYPE -o value %[1]s 2>/dev/null || true)" != "swap" ]; then
    sudo mkswap -L hotbed-swap %[1]s >/dev/null
  fi
  if ! grep -qF %[1]s /etc/fstab; then
    echo "%[1]s none swap sw,discard 0 0" | sudo tee -a /etc/fstab >/dev/null
  fi
  sudo swapon -a
fi
`, swapDiskByID)
	if _, err := sshRun(ctx, cl, swapScript); err != nil {
		return "", Result{}, fmt.Errorf("configure swap in VM: %w", err)
	}

	tsState, err = ensureVMTailscale(ctx, cl, cfg.Hostname, tsAuthKey, progress)
	if err != nil {
		return "", Result{}, err
	}

	progressf(progress, "installing Linux hotbed binary into VM at %s", vmBinPath)
	if err := sshUploadFile(ctx, cl, linuxBin, vmBinPath, "0755"); err != nil {
		return "", Result{}, err
	}

	initRes, err = initVMWorker(ctx, cl, cfg, noStart, progress)
	if err != nil {
		return "", Result{}, err
	}

	return tsState, initRes, nil
}

var tailscaleAuthURLRe = regexp.MustCompile(`https://login\.tailscale\.com/\S+`)

func ensureVMTailscale(ctx context.Context, cl *ssh.Client, hostname, authKey string, progress io.Writer) (string, error) {
	if out, err := sshRun(ctx, cl, "sudo tailscale status --json 2>/dev/null || true"); err == nil {
		if isTailscaleRunning(out) {
			summary := parseTailscaleSummary(out)
			progressf(progress, "VM is already connected to Tailscale (%s)", summary)
			return summary, nil
		}
	}

	if authKey != "" {
		progressf(progress, "authenticating VM with Tailscale auth key (hostname %s)", hostname)
		sess, err := cl.NewSession()
		if err != nil {
			return "", err
		}
		defer sess.Close()
		// Pass auth key on stdin so it never appears in process args or logs.
		sess.Stdin = strings.NewReader(authKey)
		var stderr bytes.Buffer
		sess.Stderr = &stderr
		cmd := fmt.Sprintf(`KEY=$(cat); sudo tailscale up --auth-key="$KEY" --hostname=%s`, shell.Quote(hostname))
		if err := sess.Run(cmd); err != nil {
			return "", fmt.Errorf("tailscale up in VM: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	} else {
		progressf(progress, "starting interactive Tailscale login in VM (hostname %s)", hostname)
		sess, err := cl.NewSession()
		if err != nil {
			return "", err
		}
		defer sess.Close()
		stderrPipe, err := sess.StderrPipe()
		if err != nil {
			return "", err
		}
		stdoutPipe, err := sess.StdoutPipe()
		if err != nil {
			return "", err
		}
		cmd := fmt.Sprintf("sudo tailscale up --hostname=%s", shell.Quote(hostname))
		if err := sess.Start(cmd); err != nil {
			return "", fmt.Errorf("start tailscale up: %w", err)
		}
		scanStream := func(r io.Reader) {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				line := sc.Text()
				if u := tailscaleAuthURLRe.FindString(line); u != "" {
					progressf(progress, "authenticate this worker in Tailscale by opening: %s", u)
				}
			}
		}
		go scanStream(stdoutPipe)
		go scanStream(stderrPipe)
		done := make(chan error, 1)
		go func() { done <- sess.Wait() }()
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGTERM)
			return "", ctx.Err()
		case err := <-done:
			if err != nil {
				return "", fmt.Errorf("tailscale up in VM failed: %w", err)
			}
		}
	}

	out, err := sshRun(ctx, cl, "sudo tailscale status --json")
	if err != nil {
		return "", fmt.Errorf("check tailscale status in VM: %w", err)
	}
	if !isTailscaleRunning(out) {
		return "", api.Errorf(api.CodeTailscaleUnavailable, "Tailscale inside VM did not reach Running state")
	}
	summary := parseTailscaleSummary(out)
	progressf(progress, "Tailscale connected inside VM (%s)", summary)
	return summary, nil
}

func isTailscaleRunning(rawJSON string) bool {
	var st struct {
		BackendState string `json:"BackendState"`
		Self         *struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &st); err != nil {
		return false
	}
	return st.BackendState == "Running" && st.Self != nil && len(st.Self.TailscaleIPs) > 0
}

func parseTailscaleSummary(rawJSON string) string {
	var st struct {
		BackendState string `json:"BackendState"`
		Self         *struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &st); err != nil {
		return ""
	}
	if st.Self != nil && len(st.Self.TailscaleIPs) > 0 {
		name := strings.TrimSuffix(st.Self.DNSName, ".")
		if name != "" {
			return fmt.Sprintf("%s (%s)", name, st.Self.TailscaleIPs[0])
		}
		return st.Self.TailscaleIPs[0]
	}
	return st.BackendState
}

func initVMWorker(ctx context.Context, cl *ssh.Client, cfg VMConfig, noStart bool, progress io.Writer) (Result, error) {
	return initVMWorkerWith(ctx, func(ctx context.Context, cmd string) (string, error) {
		return sshRun(ctx, cl, cmd)
	}, cfg, noStart, progress)
}

func initVMWorkerWith(ctx context.Context, run func(ctx context.Context, cmd string) (string, error), cfg VMConfig, noStart bool, progress io.Writer) (Result, error) {
	helpCmd := shell.Join([]string{"sudo", vmBinPath, "worker", "init", "--help"})
	helpOut, helpErr := run(ctx, helpCmd)
	arch := cmp.Or(cfg.Arch, runtime.GOARCH)
	skewSuggest := fmt.Sprintf("wait for CI to deploy the controller or pass `--binary /path/to/hotbed-linux-%s`", arch)
	if helpErr != nil {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "probe guest hotbed binary (%s): %v", helpCmd, helpErr).
			WithSuggestion(skewSuggest)
	}
	if !strings.Contains(helpOut, "--zfs-device") {
		return Result{}, api.Errorf(api.CodeInvalidArgs, "guest hotbed binary %s does not support --zfs-device (version skew with controller %s)", vmBinPath, cfg.Controller).
			WithSuggestion(skewSuggest)
	}

	progressf(progress, "initializing Linux worker inside VM (controller %s, ZFS device %s)", cfg.Controller, zfsDiskByID)
	out, err := run(ctx, shell.Join(VMWorkerInitArgs(cfg, noStart)))
	if err != nil {
		var env struct {
			Error *api.Error `json:"error"`
		}
		if json.Unmarshal([]byte(out), &env) == nil && env.Error != nil {
			return Result{}, env.Error
		}
		return Result{}, fmt.Errorf("hotbed worker init inside VM: %w", err)
	}
	var env struct {
		OK    bool       `json:"ok"`
		Data  *Result    `json:"data"`
		Error *api.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return Result{}, fmt.Errorf("parse hotbed worker init output: %w (%s)", err, strings.TrimSpace(out))
	}
	if !env.OK {
		if env.Error != nil {
			return Result{}, env.Error
		}
		return Result{}, fmt.Errorf("hotbed worker init returned ok=false (%s)", strings.TrimSpace(out))
	}
	if env.Data == nil {
		return Result{}, fmt.Errorf("hotbed worker init returned ok=true with missing data (%s)", strings.TrimSpace(out))
	}
	if strings.TrimSpace(env.Data.ClusterID) == "" || strings.TrimSpace(env.Data.Node) == "" {
		return Result{}, fmt.Errorf("hotbed worker init returned incomplete data (clusterId=%q, node=%q): %s", env.Data.ClusterID, env.Data.Node, strings.TrimSpace(out))
	}
	return *env.Data, nil
}

// --- disk stat helpers ---

func collectVMDisks(cfg VMConfig, p VMPaths) []DiskStat {
	var out []DiskStat
	if ds, err := FileDiskStat("zfs", p.ZFSDisk, cfg.ZFSSizeBytes); err == nil {
		out = append(out, ds)
	}
	if ds, err := FileDiskStat("root", p.RootDisk, cfg.RootSizeBytes); err == nil {
		out = append(out, ds)
	}
	if ds, err := FileDiskStat("swap", p.SwapDisk, cfg.SwapSizeBytes); err == nil {
		out = append(out, ds)
	}
	return out
}

func totalAllocated(disks []DiskStat) int64 {
	var sum int64
	for _, d := range disks {
		sum += d.AllocatedBytes
	}
	return sum
}
