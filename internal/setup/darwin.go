//go:build darwin

package setup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/shell"
	"github.com/semistrict/hotbed/internal/worker"
)

const (
	zfsDiskByID  = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs"
	swapDiskByID = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-swap"
	vmBinPath    = "/usr/local/bin/hotbed"
)

// Setup creates, boots, and provisions a QEMU Ubuntu Linux worker VM in o.Dir
// (default ~/.hotbed) with sparse discard-enabled qcow2 + ZFS storage, joins
// it to o.Controller, and starts the hotbed-worker systemd service inside it.
func Setup(ctx context.Context, o Options, _ worker.Tailnet, _ *http.Client, _ worker.Check) (Result, error) {
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
	if err := os.MkdirAll(p.VMDir, 0o755); err != nil {
		return Result{}, err
	}

	// Resolve or cross-compile the Linux hotbed binary up front so we fail fast
	// before downloading an image or booting a VM.
	linuxBin, err := resolveLinuxBinary(ctx, o.BinaryPath, p.CacheDir, arch, o.Progress)
	if err != nil {
		return Result{}, err
	}

	// Load existing VM config if re-running setup idempotently.
	var prev VMConfig
	hasPrev := readJSON(p.Config, &prev) == nil

	username := o.User
	if username == "" && hasPrev && prev.User != "" {
		username = prev.User
	}
	if username == "" {
		username, err = DefaultUser()
		if err != nil {
			return Result{}, err
		}
	}
	hostname := o.Hostname
	if hostname == "" && hasPrev && prev.Hostname != "" {
		hostname = prev.Hostname
	}
	if hostname == "" {
		hostname = DefaultVMHostname()
	}
	hostname = SanitizeHostname(hostname)

	ubuntuVer := o.UbuntuVersion
	if ubuntuVer == "" && hasPrev && prev.UbuntuVersion != "" {
		ubuntuVer = prev.UbuntuVersion
	}
	if ubuntuVer == "" {
		ubuntuVer = DefaultUbuntuVersion
	}

	cpus := o.CPUs
	if cpus <= 0 && hasPrev && prev.CPUs > 0 {
		cpus = prev.CPUs
	}
	if cpus <= 0 {
		cpus = defaultVMCPUs()
	}

	memBytes := o.MemoryBytes
	if memBytes <= 0 && hasPrev && prev.MemoryBytes > 0 {
		memBytes = prev.MemoryBytes
	}
	if memBytes <= 0 {
		memBytes = defaultVMMemory()
	}

	rootSize := o.RootSize
	if rootSize <= 0 && hasPrev && prev.RootSizeBytes > 0 {
		rootSize = prev.RootSizeBytes
	}
	if rootSize <= 0 {
		rootSize = DefaultRootSize
	}

	zfsSize := o.DiskSize
	if zfsSize <= 0 && hasPrev && prev.ZFSSizeBytes > 0 {
		zfsSize = prev.ZFSSizeBytes
	}
	if zfsSize <= 0 {
		zfsSize = DefaultDiskSize
	}

	swapSize := o.SwapSize
	if swapSize <= 0 && hasPrev && prev.SwapSizeBytes > 0 {
		swapSize = prev.SwapSizeBytes
	}
	if swapSize <= 0 {
		swapSize = memBytes
	}

	sshPort := o.SSHPort
	if sshPort <= 0 && hasPrev && prev.SSHPort > 0 {
		sshPort = prev.SSHPort
	}
	if sshPort <= 0 {
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
		TmpDir:         o.TmpDir,
		Autostart:      !o.NoAutostart,
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

	if err := ensureSeedISO(ctx, p.SeedISO, hostname, username, pubKeys); err != nil {
		return Result{}, err
	}
	if err := ensureVMDisks(ctx, qemuImg, cachedImg, fwVarsTemplate, cfg, p, o.Progress); err != nil {
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

	tsState, joinRes, err := provisionVM(ctx, sshClient, cfg, linuxBin, o.TailscaleAuthKey, o.NoStart, o.Progress)
	if err != nil {
		return Result{}, err
	}

	if cfg.Autostart {
		if err := installLaunchAgent(cfg, p); err != nil {
			progressf(o.Progress, "warning: could not write LaunchAgent: %v", err)
			cfg.Autostart = false
			_ = writeJSONAtomic(p.Config, cfg, 0o644)
		}
	}

	if o.NoStart {
		if err := stopVM(ctx, cfg, p, o.Progress); err != nil {
			return Result{}, err
		}
	}

	pid, running := vmProcessState(ctx, p)
	disks := collectVMDisks(cfg, p)
	return Result{
		Platform:   "darwin-qemu",
		Dir:        baseDir,
		StateDir:   cfg.StateDir,
		Controller: controller,
		User:       username,
		Node:       joinRes.Node,
		ClusterID:  joinRes.ClusterID,
		Storage:    "zfs-qcow2",
		Running:    running && !o.NoStart,
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
			Tailscale:  tsState,
			Autostart:  cfg.Autostart,
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

// workerRecord is the file worker init records the VM's setup in, its
// controller among it.
func workerRecord(baseDir string) string { return NewVMPaths(baseDir).Config }

// GetStatus reports the local macOS QEMU worker VM's state and sparse disk usage.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir).
				WithSuggestion("run `hotbed worker init CONTROLLER`")
		}
		return Status{}, err
	}
	pid, running := vmProcessState(ctx, p)
	workerRunning := false
	tsState := ""
	if running {
		if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
			if cl, err := dialSSH(ctx, cfg, signer, 3*time.Second); err == nil {
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
			Tailscale:  tsState,
			Autostart:  cfg.Autostart,
			ConsoleLog: p.ConsoleLog,
		},
		Disks: collectVMDisks(cfg, p),
	}, nil
}

// Start boots the configured macOS QEMU worker VM if it is not already running.
func Start(ctx context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir).
				WithSuggestion("run `hotbed worker init CONTROLLER`")
		}
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
	return GetStatus(ctx, baseDir)
}

// Stop gracefully shuts down the macOS QEMU worker VM so ZFS exports cleanly.
func Stop(ctx context.Context, dir string) (Status, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir)
		}
		return Status{}, err
	}
	if err := stopVM(ctx, cfg, p, os.Stderr); err != nil {
		return Status{}, err
	}
	return GetStatus(ctx, baseDir)
}

// Trim runs `zpool trim -w hotbed` and `fstrim -a` inside the VM to punch freed
// clusters out of the host's qcow2 sparse images on APFS.
func Trim(ctx context.Context, dir string) (TrimResult, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return TrimResult{}, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TrimResult{}, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir)
		}
		return TrimResult{}, err
	}
	if _, running := vmProcessState(ctx, p); !running {
		return TrimResult{}, api.Errorf(api.CodeConflict, "worker VM is not running").
			WithSuggestion("start it with `hotbed worker start` before trimming")
	}
	signer, err := loadSSHSigner(p.KeyPriv)
	if err != nil {
		return TrimResult{}, err
	}
	cl, err := dialSSH(ctx, cfg, signer, 10*time.Second)
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
	if err := readJSON(p.Config, &cfg); err == nil {
		_ = stopVM(ctx, cfg, p, o.Progress)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return DestroyResult{}, err
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
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return 1, err
	}
	p := NewVMPaths(baseDir)
	var cfg VMConfig
	if err := readJSON(p.Config, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 1, api.Errorf(api.CodeNotInitialized, "no worker VM configured in %s", baseDir).
				WithSuggestion("run `hotbed worker init CONTROLLER`")
		}
		return 1, err
	}
	if _, running := vmProcessState(ctx, p); !running {
		return 1, api.Errorf(api.CodeConflict, "worker VM is not running").
			WithSuggestion("start it with `hotbed worker start`")
	}
	sshArgs := []string{
		"-i", p.KeyPriv,
		"-p", strconv.Itoa(cfg.SSHPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
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

func defaultVMMemory() int64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 8 << 30
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || total <= 0 {
		return 8 << 30
	}
	mem := total / 2
	const (
		minMem = 4 << 30
		maxMem = 32 << 30
	)
	if mem < minMem {
		mem = minMem
	}
	if mem > maxMem {
		mem = maxMem
	}
	// Round to whole GiB.
	return (mem / (1 << 30)) * (1 << 30)
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

// --- Linux binary resolution & cross-compilation ---

func resolveLinuxBinary(ctx context.Context, explicit, cacheDir, arch string, progress io.Writer) (string, error) {
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

	// If running from a hotbed source checkout with Go installed, cross-compile
	// a fresh static Linux binary into cacheDir.
	if repoRoot := findHotbedSourceRoot(); repoRoot != "" {
		if goBin, err := exec.LookPath("go"); err == nil {
			outPath := filepath.Join(cacheDir, "hotbed-linux-"+arch)
			tmpPath := outPath + ".tmp"
			progressf(progress, "cross-compiling Linux %s hotbed binary from %s", arch, repoRoot)
			cmd := exec.CommandContext(ctx, goBin, "build", "-o", tmpPath, "./cmd/hotbed")
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				os.Remove(tmpPath)
				return "", fmt.Errorf("cross-compile linux/%s hotbed binary: %w: %s", arch, err, strings.TrimSpace(stderr.String()))
			}
			if err := os.Rename(tmpPath, outPath); err != nil {
				os.Remove(tmpPath)
				return "", err
			}
			return outPath, nil
		}
	}

	// Check sibling binary next to current executable or previously cached binary.
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "hotbed-linux-"+arch))
	}
	candidates = append(candidates, filepath.Join(cacheDir, "hotbed-linux-"+arch))
	for _, c := range candidates {
		if verifyLinuxELF(c, arch) == nil {
			progressf(progress, "using Linux %s hotbed binary %s", arch, c)
			return c, nil
		}
	}

	return "", api.Errorf(api.CodeInvalidArgs, "no Linux %s hotbed binary found to install inside the VM", arch).
		WithSuggestion(fmt.Sprintf("pass --binary /path/to/hotbed-linux-%s, or run from the hotbed Go source tree with `go` installed", arch))
}

func findHotbedSourceRoot() string {
	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			exe = realExe
		}
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		dir := start
		for {
			goMod := filepath.Join(dir, "go.mod")
			if b, err := os.ReadFile(goMod); err == nil {
				if strings.Contains(string(b), "module github.com/semistrict/hotbed") {
					if _, err := os.Stat(filepath.Join(dir, "cmd", "hotbed")); err == nil {
						return dir
					}
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

func verifyLinuxELF(path, arch string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("read ELF header: %w", err)
	}
	if hdr[0] != 0x7f || hdr[1] != 'E' || hdr[2] != 'L' || hdr[3] != 'F' {
		return errors.New("not a Linux ELF executable")
	}
	// Little-endian e_machine at offset 18: 183 = EM_AARCH64, 62 = EM_X86_64.
	machine := binary.LittleEndian.Uint16(hdr[18:20])
	want := uint16(183)
	if arch == "amd64" {
		want = 62
	}
	if machine != want {
		return fmt.Errorf("ELF machine %d does not match guest arch %s (want %d)", machine, arch, want)
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

func ensureSeedISO(ctx context.Context, isoPath, hostname, username string, pubKeys []string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(isoPath), "seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, "meta-data"), []byte(CloudInitMetaData(hostname)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "user-data"), []byte(CloudInitUserData(hostname, username, pubKeys)), 0o644); err != nil {
		return err
	}
	_ = os.Remove(isoPath)
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "makehybrid",
		"-iso", "-joliet", "-default-volume-name", "cidata",
		"-o", isoPath, tmpDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build cloud-init seed ISO: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func ensureVMDisks(ctx context.Context, qemuImg, cachedImg, fwVarsTemplate string, cfg VMConfig, p VMPaths, progress io.Writer) error {
	if _, err := os.Stat(p.RootDisk); errors.Is(err, fs.ErrNotExist) {
		progressf(progress, "creating sparse root disk %s (%s virtual)", p.RootDisk, FormatBytes(cfg.RootSizeBytes))
		if err := copyFile(cachedImg, p.RootDisk, 0o644); err != nil {
			return fmt.Errorf("create root disk: %w", err)
		}
		if err := runQEMUImg(ctx, qemuImg, "resize", p.RootDisk, strconv.FormatInt(cfg.RootSizeBytes, 10)); err != nil {
			os.Remove(p.RootDisk)
			return err
		}
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
			if err := copyFile(fwVarsTemplate, p.EFIVars, 0o644); err != nil {
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

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// --- QEMU process lifecycle ---

// qmpSocketPath returns p.QMPSock unless it would exceed macOS's 104-byte
// Unix domain socket path limit, in which case it uses a short /tmp path.
func qmpSocketPath(p VMPaths) string {
	if len(p.QMPSock) < 100 {
		return p.QMPSock
	}
	sum := sha256.Sum256([]byte(p.VMDir))
	return filepath.Join("/tmp", "hotbed-qmp-"+hex.EncodeToString(sum[:6])+".sock")
}

func vmProcessState(ctx context.Context, p VMPaths) (pid int, running bool) {
	b, err := os.ReadFile(p.PIDFile)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if syscall.Kill(pid, 0) != nil {
		return 0, false
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := QMPQueryStatus(qctx, qmpSocketPath(p)); err == nil {
		return pid, true
	}
	// Process exists even if QMP is still starting up.
	return pid, true
}

func ensureVMRunning(ctx context.Context, cfg VMConfig, p VMPaths, progress io.Writer) error {
	if pid, running := vmProcessState(ctx, p); running {
		progressf(progress, "worker VM is already running (pid %d)", pid)
		return nil
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
	pid, running := vmProcessState(ctx, p)
	if !running {
		_ = os.Remove(p.PIDFile)
		_ = os.Remove(qmpSocketPath(p))
		return nil
	}
	progressf(progress, "shutting down worker VM (pid %d)", pid)

	// Prefer clean guest poweroff over SSH so ZFS exports cleanly; fall back to QMP ACPI powerdown.
	poweredDown := false
	if signer, err := loadSSHSigner(p.KeyPriv); err == nil {
		if cl, err := dialSSH(ctx, cfg, signer, 3*time.Second); err == nil {
			_, _ = sshRun(ctx, cl, "sudo systemctl poweroff || sudo poweroff")
			cl.Close()
			poweredDown = true
		}
	}
	if !poweredDown {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = QMPPowerdown(qctx, qmpSocketPath(p))
		cancel()
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			_ = os.Remove(p.PIDFile)
			_ = os.Remove(qmpSocketPath(p))
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(p.PIDFile)
	_ = os.Remove(qmpSocketPath(p))
	return nil
}

// --- SSH provisioning inside the Ubuntu guest ---

func dialSSH(ctx context.Context, cfg VMConfig, signer ssh.Signer, timeout time.Duration) (*ssh.Client, error) {
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
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
		if _, running := vmProcessState(ctx, p); !running {
			logTail, _ := os.ReadFile(p.QEMULog)
			return nil, fmt.Errorf("worker VM exited while booting (see %s): %s", p.ConsoleLog, strings.TrimSpace(string(logTail)))
		}
		if cl, err := dialSSH(ctx, cfg, signer, 3*time.Second); err == nil {
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

func provisionVM(ctx context.Context, cl *ssh.Client, cfg VMConfig, linuxBin, tsAuthKey string, noStart bool, progress io.Writer) (tsState string, joinRes worker.JoinResult, err error) {
	progressf(progress, "waiting for cloud-init inside VM")
	if _, err := sshRun(ctx, cl, "sudo cloud-init status --wait >/dev/null 2>&1 || true"); err != nil {
		return "", worker.JoinResult{}, err
	}

	progressf(progress, "installing ZFS, git, build tools, and Tailscale inside Ubuntu VM")
	pkgScript := `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v zpool >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1 || ! command -v gcc >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq zfsutils-linux git curl ca-certificates build-essential pkg-config
fi
if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sudo sh
fi
sudo systemctl enable --now tailscaled
`
	if _, err := sshRun(ctx, cl, pkgScript); err != nil {
		return "", worker.JoinResult{}, fmt.Errorf("install packages in VM: %w", err)
	}

	progressf(progress, "configuring sparse ZFS pool (%s, autotrim=on, compression=zstd) and discard swap", DefaultPoolName)
	storageScript := fmt.Sprintf(`set -euo pipefail
sudo modprobe zfs
# Promptly punch out freed ZFS extents via autotrim instead of batching 32 TXGs.
echo "options zfs zfs_trim_txg_batch=1" | sudo tee /etc/modprobe.d/hotbed-zfs.conf >/dev/null
if [ -w /sys/module/zfs/parameters/zfs_trim_txg_batch ]; then
  echo 1 | sudo tee /sys/module/zfs/parameters/zfs_trim_txg_batch >/dev/null
fi
if ! sudo zpool list -H -o name %[1]s >/dev/null 2>&1; then
  if ! sudo zpool import -f %[1]s >/dev/null 2>&1; then
    sudo zpool create -f \
      -o ashift=12 \
      -o autotrim=on \
      -o cachefile=/etc/zfs/zpool.cache \
      -O compression=zstd \
      -O atime=off \
      -O xattr=sa \
      -O dnodesize=auto \
      -m %[2]s \
      %[1]s %[3]s
  fi
fi
sudo zpool set autotrim=on %[1]s
sudo systemctl enable zfs-import-cache.service zfs-mount.service zfs.target >/dev/null 2>&1 || true

# Configure sparse discard swap disk so parked commands swap cleanly and punch out when thawed.
if ! swapon --show=NAME --noheadings | grep -q .; then
  if [ "$(sudo blkid -s TYPE -o value %[4]s 2>/dev/null || true)" != "swap" ]; then
    sudo mkswap -L hotbed-swap %[4]s >/dev/null
  fi
  if ! grep -qF %[4]s /etc/fstab; then
    echo "%[4]s none swap sw,discard 0 0" | sudo tee -a /etc/fstab >/dev/null
  fi
  sudo swapon -a
fi
`, DefaultPoolName, shell.Quote(cfg.StateDir), shell.Quote(zfsDiskByID), swapDiskByID)
	if _, err := sshRun(ctx, cl, storageScript); err != nil {
		return "", worker.JoinResult{}, fmt.Errorf("configure ZFS and swap in VM: %w", err)
	}

	// Install hourly trim timer inside the VM so freed ZFS/ext4 space gets punched out on APFS.
	trimSvc, trimTmr := SystemdTrimUnits(DefaultPoolName, true)
	trimInstall := fmt.Sprintf(`set -euo pipefail
cat <<'EOF' | sudo tee /etc/systemd/system/hotbed-trim.service >/dev/null
%sEOF
cat <<'EOF' | sudo tee /etc/systemd/system/hotbed-trim.timer >/dev/null
%sEOF
sudo systemctl daemon-reload
sudo systemctl enable --now hotbed-trim.timer
`, trimSvc, trimTmr)
	if _, err := sshRun(ctx, cl, trimInstall); err != nil {
		return "", worker.JoinResult{}, fmt.Errorf("install trim timer in VM: %w", err)
	}

	progressf(progress, "installing Linux hotbed binary into VM at %s", vmBinPath)
	if err := sshUploadFile(ctx, cl, linuxBin, vmBinPath, "0755"); err != nil {
		return "", worker.JoinResult{}, err
	}

	tsState, err = ensureVMTailscale(ctx, cl, cfg.Hostname, tsAuthKey, progress)
	if err != nil {
		return "", worker.JoinResult{}, err
	}

	joinRes, err = ensureVMJoined(ctx, cl, cfg, progress)
	if err != nil {
		return "", worker.JoinResult{}, err
	}

	unitText := SystemdWorkerUnit(vmBinPath, cfg.StateDir, "")
	svcScript := fmt.Sprintf(`set -euo pipefail
cat <<'EOF' | sudo tee /etc/systemd/system/hotbed-worker.service >/dev/null
%sEOF
sudo systemctl daemon-reload
sudo systemctl enable hotbed-worker.service
`, unitText)
	if !noStart {
		svcScript += "sudo systemctl restart hotbed-worker.service\nsudo systemctl is-active hotbed-worker.service\n"
	}
	progressf(progress, "configuring hotbed-worker.service inside VM")
	if _, err := sshRun(ctx, cl, svcScript); err != nil {
		return "", worker.JoinResult{}, fmt.Errorf("configure hotbed-worker.service in VM: %w", err)
	}

	return tsState, joinRes, nil
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

func ensureVMJoined(ctx context.Context, cl *ssh.Client, cfg VMConfig, progress io.Writer) (worker.JoinResult, error) {
	// Check if already joined inside the VM.
	if out, err := sshRun(ctx, cl, fmt.Sprintf("sudo cat %s 2>/dev/null || true", shell.Quote(filepath.Join(cfg.StateDir, "hotbed.json")))); err == nil && strings.TrimSpace(out) != "" {
		var sc struct {
			Worker *struct {
				ClusterID  string `json:"clusterId"`
				Controller string `json:"controller"`
				User       string `json:"user"`
			} `json:"worker"`
		}
		if json.Unmarshal([]byte(out), &sc) == nil && sc.Worker != nil {
			if sc.Worker.Controller == cfg.Controller {
				progressf(progress, "VM worker is already joined to %s", cfg.Controller)
				return worker.JoinResult{
					ClusterID:  sc.Worker.ClusterID,
					Dir:        cfg.StateDir,
					Controller: sc.Worker.Controller,
					Storage:    "zfs",
					User:       sc.Worker.User,
					Node:       cfg.Hostname,
				}, nil
			}
			return worker.JoinResult{}, api.Errorf(api.CodeAlreadyInitialized, "VM worker is already joined to %s (want %s)", sc.Worker.Controller, cfg.Controller).
				WithSuggestion("destroy and recreate with `hotbed worker destroy`")
		}
	}

	progressf(progress, "joining controller %s from inside VM", cfg.Controller)
	args := []string{
		"sudo", vmBinPath, "join",
		cfg.Controller, cfg.StateDir,
		"--user", cfg.User,
	}
	if cfg.Slice != "" {
		args = append(args, "--slice", cfg.Slice)
	}
	if cfg.TmpDir != "" {
		args = append(args, "--tmp-dir", cfg.TmpDir)
	}
	for _, prop := range cfg.UnitProperties {
		args = append(args, "--unit-property", prop)
	}
	out, err := sshRun(ctx, cl, shell.Join(args))
	if err != nil {
		// Try decoding hotbed's JSON envelope error for a clean API error.
		var env struct {
			Error *api.Error `json:"error"`
		}
		if json.Unmarshal([]byte(out), &env) == nil && env.Error != nil {
			return worker.JoinResult{}, env.Error
		}
		return worker.JoinResult{}, fmt.Errorf("hotbed join inside VM: %w", err)
	}
	var env struct {
		OK    bool              `json:"ok"`
		Data  worker.JoinResult `json:"data"`
		Error *api.Error        `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return worker.JoinResult{}, fmt.Errorf("parse hotbed join output: %w (%s)", err, strings.TrimSpace(out))
	}
	if !env.OK && env.Error != nil {
		return worker.JoinResult{}, env.Error
	}
	return env.Data, nil
}

// --- macOS LaunchAgent & disk stat helpers ---

func launchAgentLabel(baseDir string) string {
	def, err := DefaultDir()
	if err == nil && filepath.Clean(baseDir) == filepath.Clean(def) {
		return "com.semistrict.hotbed.worker"
	}
	sum := sha256.Sum256([]byte(filepath.Clean(baseDir)))
	return "com.semistrict.hotbed.worker." + hex.EncodeToString(sum[:4])
}

func launchAgentPath(baseDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel(baseDir)+".plist")
}

func installLaunchAgent(cfg VMConfig, p VMPaths) error {
	plistPath := launchAgentPath(p.BaseDir)
	if plistPath == "" {
		return errors.New("could not determine LaunchAgents directory")
	}
	args, err := QEMUArgs(cfg, p, qmpSocketPath(p))
	if err != nil {
		return err
	}
	content := LaunchAgentPlist(launchAgentLabel(p.BaseDir), cfg.QEMUBinary, args, p.QEMULog)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(plistPath, []byte(content), 0o644)
}

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
