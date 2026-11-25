package setup

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VMConfig is persisted in <dir>/vm/config.json on macOS.
type VMConfig struct {
	Controller     string   `json:"controller"`
	User           string   `json:"user"`
	Hostname       string   `json:"hostname"`
	Arch           string   `json:"arch"` // "arm64" or "amd64"
	UbuntuVersion  string   `json:"ubuntuVersion"`
	ImageURL       string   `json:"imageUrl"`
	CPUs           int      `json:"cpus"`
	MemoryBytes    int64    `json:"memoryBytes"`
	RootSizeBytes  int64    `json:"rootSizeBytes"`
	ZFSSizeBytes   int64    `json:"zfsSizeBytes"`
	SwapSizeBytes  int64    `json:"swapSizeBytes"`
	VfkitBinary    string   `json:"vfkitBinary"`
	StateDir       string   `json:"stateDir"`
	UnitProperties []string `json:"unitProperties,omitempty"`
	Slice          string   `json:"slice,omitempty"`
	// Autostart has a LaunchAgent supervise the VM (hotbed worker
	// supervise): start it when a run needs it, power it off once idle.
	Autostart bool `json:"autostart"`
	// SleepAfter is how long the VM has to have had no work before it is
	// powered off (a Go duration); empty for DefaultSleepAfter, 0 never.
	SleepAfter string `json:"sleepAfter,omitempty"`
}

// SleepAfterDuration is cfg.SleepAfter, DefaultSleepAfter when empty.
func (cfg VMConfig) SleepAfterDuration() (time.Duration, error) {
	if cfg.SleepAfter == "" {
		return DefaultSleepAfter, nil
	}
	d, err := time.ParseDuration(cfg.SleepAfter)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("sleepAfter %q is not a duration of 0 or more, such as 30m", cfg.SleepAfter)
	}
	return d, nil
}

// VMPaths holds every file path for a VM inside <baseDir>/vm.
type VMPaths struct {
	BaseDir     string
	CacheDir    string
	VMDir       string
	Config      string
	KeyPriv     string
	KeyPub      string
	HostKeyPriv string
	HostKeyPub  string
	RootDisk    string
	ZFSDisk     string
	SwapDisk    string
	EFIVars     string
	SeedISO     string
	// ControlSock is vfkit's REST API (VM state, clean and hard stop).
	ControlSock string
	// SSHSock is the host end of the guest's vsock port 22, where systemd's
	// sshd-vsock.socket answers.
	SSHSock    string
	PIDFile    string
	ConsoleLog string
	VfkitLog   string
	// StandbyLog is the VM's supervisor's log (hotbed worker supervise).
	StandbyLog    string
	StoppedMarker string
	AsleepMarker  string
}

// NewVMPaths computes the standard layout under baseDir (typically ~/.hotbed).
func NewVMPaths(baseDir string) VMPaths {
	vmDir := filepath.Join(baseDir, "vm")
	return VMPaths{
		BaseDir:       baseDir,
		CacheDir:      filepath.Join(baseDir, "cache"),
		VMDir:         vmDir,
		Config:        filepath.Join(vmDir, "config.json"),
		KeyPriv:       filepath.Join(vmDir, "id_ed25519"),
		KeyPub:        filepath.Join(vmDir, "id_ed25519.pub"),
		HostKeyPriv:   filepath.Join(vmDir, "vm-ssh-host-ed25519"),
		HostKeyPub:    filepath.Join(vmDir, "vm-ssh-host-ed25519.pub"),
		RootDisk:      filepath.Join(vmDir, "root.img"),
		ZFSDisk:       filepath.Join(vmDir, "zfs.img"),
		SwapDisk:      filepath.Join(vmDir, "swap.img"),
		EFIVars:       filepath.Join(vmDir, "efi-vars"),
		SeedISO:       filepath.Join(vmDir, "seed.iso"),
		ControlSock:   filepath.Join(vmDir, "vfkit.sock"),
		SSHSock:       filepath.Join(vmDir, "ssh.sock"),
		PIDFile:       filepath.Join(vmDir, "vfkit.pid"),
		ConsoleLog:    filepath.Join(vmDir, "console.log"),
		VfkitLog:      filepath.Join(vmDir, "vfkit.log"),
		StandbyLog:    filepath.Join(vmDir, "supervise.log"),
		StoppedMarker: filepath.Join(vmDir, "stopped"),
		AsleepMarker:  filepath.Join(vmDir, "asleep"),
	}
}

// UbuntuCloudImageURL returns the official cloud image URL, filename, and
// SHA256SUMS URL for the given Ubuntu version (e.g. "26.04") and arch ("arm64"
// or "amd64").
func UbuntuCloudImageURL(version, arch string) (imageURL, filename, sumsURL string) {
	if version == "" {
		version = DefaultUbuntuVersion
	}
	filename = fmt.Sprintf("ubuntu-%s-server-cloudimg-%s.img", version, arch)
	base := fmt.Sprintf("https://cloud-images.ubuntu.com/releases/%s/release", version)
	return base + "/" + filename, filename, base + "/SHA256SUMS"
}

// ParseSHA256Sums extracts the SHA-256 hex digest for filename from a GNU
// sha256sum file (such as Ubuntu's SHA256SUMS).
func ParseSHA256Sums(content, filename string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = strings.TrimPrefix(name, "./")
		if name == filename {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// Guest disk identifiers: virtio-blk deviceIds, which the guest lists as
// /dev/disk/by-id/virtio-<id>.
const (
	rootDiskID = "hotbed-root"
	zfsDiskID  = "hotbed-zfs"
	swapDiskID = "hotbed-swap"
	seedDiskID = "hotbed-seed"
)

// VfkitArgs builds the vfkit command line for cfg: an Apple Virtualization
// framework VM with nested virtualization (so the guest has /dev/kvm for
// Firecracker), booted by EFI from the root disk. Disks are sparse raw files:
// guest TRIM (ZFS autotrim, fstrim, swapon --discard) punches holes out of
// them on APFS. controlSock serves vfkit's REST API; sshSock is the host end
// of guest vsock port 22, where systemd's sshd-vsock.socket answers.
// createEFIVars has vfkit create the EFI variable store, which it must only
// when there is none: creating one replaces what the guest keeps there.
func VfkitArgs(cfg VMConfig, p VMPaths, controlSock, sshSock string, createEFIVars bool) ([]string, error) {
	if cfg.Arch != "arm64" {
		return nil, fmt.Errorf("unsupported VM architecture %q: the worker VM needs nested virtualization, which the Virtualization framework has only on Apple silicon", cfg.Arch)
	}
	if cfg.CPUs <= 0 {
		return nil, fmt.Errorf("invalid VM CPU count %d", cfg.CPUs)
	}
	memMB := cfg.MemoryBytes / (1 << 20)
	if memMB < 512 {
		return nil, fmt.Errorf("VM memory %d MiB is too small (minimum 512 MiB)", memMB)
	}
	disk := func(path, id string) string { return "virtio-blk,path=" + path + ",deviceId=" + id }
	efi := "efi,variable-store=" + p.EFIVars
	if createEFIVars {
		efi += ",create"
	}
	return []string{
		"--cpus", strconv.Itoa(cfg.CPUs),
		"--memory", strconv.FormatInt(memMB, 10),
		"--nested",
		"--pidfile", p.PIDFile,
		"--restful-uri", "unix://" + controlSock,
		"--bootloader", efi,
		"--device", disk(p.RootDisk, rootDiskID),
		"--device", disk(p.ZFSDisk, zfsDiskID),
		"--device", disk(p.SwapDisk, swapDiskID),
		"--device", disk(p.SeedISO, seedDiskID),
		"--device", "virtio-net,nat",
		"--device", "virtio-vsock,port=22,socketURL=" + sshSock + ",connect",
		"--device", "virtio-serial,logFilePath=" + p.ConsoleLog,
		"--device", "virtio-rng",
	}, nil
}

// CloudInitMetaData renders the NoCloud meta-data YAML.
func CloudInitMetaData(hostname string) string {
	return fmt.Sprintf("instance-id: hotbed-%s\nlocal-hostname: %s\n", hostname, hostname)
}

// VMHostKey holds an Ed25519 SSH host keypair embedded in cloud-init user-data
// so the host can verify the VM's SSH identity from first boot.
type VMHostKey struct {
	PrivatePEM string
	Public     string
}

// CloudInitUserData renders the NoCloud user-data YAML that boots SSH quickly
// with the generated management key, any host public keys, and an optional
// seeded Ed25519 SSH host keypair.
func CloudInitUserData(hostname, username string, pubKeys []string, hostKeys ...VMHostKey) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", hostname)
	b.WriteString("manage_etc_hosts: true\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("package_update: false\n")
	b.WriteString("package_upgrade: false\n")
	if len(hostKeys) > 0 {
		hk := hostKeys[0]
		priv := strings.TrimRight(hk.PrivatePEM, "\r\n")
		pub := strings.TrimSpace(hk.Public)
		if priv != "" && pub != "" {
			b.WriteString("ssh_keys:\n")
			b.WriteString("  ed25519_private: |\n")
			for _, line := range strings.Split(priv, "\n") {
				fmt.Fprintf(&b, "    %s\n", strings.TrimRight(line, "\r"))
			}
			fmt.Fprintf(&b, "  ed25519_public: %s\n", pub)
		}
	}
	b.WriteString("users:\n")
	fmt.Fprintf(&b, "  - name: %s\n", username)
	b.WriteString("    groups: [sudo]\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n")
	b.WriteString("    lock_passwd: true\n")
	if len(pubKeys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, k := range pubKeys {
			k = strings.TrimSpace(k)
			if k != "" {
				fmt.Fprintf(&b, "      - %s\n", k)
			}
		}
	}
	return b.String()
}

// SystemdWorkerUnit renders the systemd unit file for hotbed-worker.service.
// When zfsImportPre is non-empty (file-backed ZFS pool on Linux), it runs as
// ExecStartPre so the pool imports before hotbed serve starts.
func SystemdWorkerUnit(binaryPath, stateDir, zfsImportPre string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=hotbed worker\n")
	b.WriteString("After=network-online.target tailscaled.service zfs-mount.service\n")
	b.WriteString("Wants=network-online.target tailscaled.service zfs-mount.service\n\n")
	b.WriteString("[Service]\n")
	if zfsImportPre != "" {
		fmt.Fprintf(&b, "ExecStartPre=-%s\n", zfsImportPre)
	}
	fmt.Fprintf(&b, "ExecStart=%s serve %s\n", systemdQuote(binaryPath), systemdQuote(stateDir))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("LimitNOFILE=1048576\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// SystemdTrimUnits renders the periodic trim service and timer that flushes
// ZFS async/deferred frees so autotrim=on punches freed extents out of sparse
// backing files / disk images without initializing untouched metaslabs, and
// runs `fstrim -a` when fstrimAll is true (for the VM's ext4 root disk).
func SystemdTrimUnits(poolName string, fstrimAll bool) (service, timer string) {
	cmd := fmt.Sprintf("zpool wait -t free %[1]s || true; zpool sync %[1]s || true; zpool sync %[1]s || true; zpool sync %[1]s || true", poolName)
	if fstrimAll {
		cmd += "; fstrim -a || true"
	}
	service = fmt.Sprintf(`[Unit]
Description=Reclaim free space from hotbed sparse storage

[Service]
Type=oneshot
ExecStart=/bin/sh -c '%s'
`, cmd)
	timer = `[Unit]
Description=Periodically trim hotbed sparse storage

[Timer]
OnBootSec=15m
OnUnitActiveSec=1h
Persistent=true

[Install]
WantedBy=timers.target
`
	return service, timer
}

// supervisorExitTimeout is how many seconds launchd gives the supervisor to
// exit before it kills it: more than stopVM takes at most (30s for the guest
// to power off, then SIGTERM and 2s).
const supervisorExitTimeout = 60

// LaunchAgentPlist renders the macOS LaunchAgent plist that runs program
// with args from login on, and again whenever it exits: the worker VM's
// supervisor.
func LaunchAgentPlist(label, program string, args []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlEscape(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(program))
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	// The VM it started runs on when it exits, as for a new build.
	b.WriteString("  <key>AbandonProcessGroup</key>\n  <true/>\n")
	// Told to exit while it powers the VM off, it finishes first.
	fmt.Fprintf(&b, "  <key>ExitTimeOut</key>\n  <integer>%d</integer>\n", supervisorExitTimeout)
	fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n", xmlEscape(logPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n", xmlEscape(logPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\") {
		return s
	}
	return strconv.Quote(s)
}

// LinuxSetupMeta is persisted in root-owned /etc/hotbed/worker-*.json on Linux
// so status, start, stop, trim and destroy know how the worker was configured.
type LinuxSetupMeta struct {
	StateDir    string `json:"stateDir"`
	User        string `json:"user"`
	Controller  string `json:"controller"`
	StorageMode string `json:"storageMode"` // "btrfs", "zfs", "zfs-file", "zfs-device"
	PoolName    string `json:"poolName,omitempty"`
	ImagePath   string `json:"imagePath,omitempty"`
	ZFSDevice   string `json:"zfsDevice,omitempty"`
	DiskSize    int64  `json:"diskSize,omitempty"`
	ServiceName string `json:"serviceName"`
	TrimTimer   string `json:"trimTimer,omitempty"`
}

const (
	// vmBinPath is where the worker VM's hotbed binary is installed.
	vmBinPath = "/usr/local/bin/hotbed"
	// Guest /dev/disk/by-id paths of VfkitArgs's disks.
	zfsDiskByID  = "/dev/disk/by-id/virtio-" + zfsDiskID
	swapDiskByID = "/dev/disk/by-id/virtio-" + swapDiskID
)

// DefaultVMMemory is the RAM a new worker VM gets on a Mac with hostBytes of
// it: a quarter, from 4 GiB to 16 GiB. The Mac never gets back memory the
// guest has touched while the VM runs (dead-ends/
// virtio-balloon-free-page-reporting-on-macos.md), so a VM given more ends
// up held, dirty, in the Mac's compressor, starving the Mac and the VM both.
func DefaultVMMemory(hostBytes int64) int64 {
	mem := min(max(hostBytes/4, 4<<30), 16<<30)
	return mem >> 30 << 30 // whole GiB
}

// VMZFSARCMax caps the worker VM's ZFS ARC at a quarter of its RAM. zfs's
// own cap is nearly all of it, and every page the ARC cycles through is one
// the Mac then holds; with the disk images on the Mac's page cache, what the
// ARC drops is still cached, as clean pages the Mac can drop instead of
// compressing.
func VMZFSARCMax(memBytes int64) int64 {
	return memBytes / 4
}

// VMWorkerInitArgs is the command that provisions the Linux worker inside the
// macOS worker VM via `hotbed worker init --zfs-device`. Its JSON envelope is
// parsed, so it asks for one: hotbed prints plain text unless told otherwise.
func VMWorkerInitArgs(cfg VMConfig, noStart bool) []string {
	args := []string{
		"sudo", vmBinPath, "--json", "worker", "init",
		cfg.Controller,
		"--dir", cfg.StateDir,
		"--user", cfg.User,
		"--zfs-device", zfsDiskByID,
		"--fstrim",
		"--zfs-arc-max", strconv.FormatInt(VMZFSARCMax(cfg.MemoryBytes), 10),
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname", cfg.Hostname)
	}
	if cfg.Slice != "" {
		args = append(args, "--slice", cfg.Slice)
	}
	for _, prop := range cfg.UnitProperties {
		args = append(args, "--unit-property", prop)
	}
	if noStart {
		args = append(args, "--no-start")
	}
	return args
}
