package setup

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
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
	SSHPort        int      `json:"sshPort"`
	QEMUBinary     string   `json:"qemuBinary"`
	FirmwareCode   string   `json:"firmwareCode"`
	StateDir       string   `json:"stateDir"`
	UnitProperties []string `json:"unitProperties,omitempty"`
	Slice          string   `json:"slice,omitempty"`
	TmpDir         string   `json:"tmpDir,omitempty"`
	Autostart      bool     `json:"autostart"`
}

// VMPaths holds every file path for a VM inside <baseDir>/vm.
type VMPaths struct {
	BaseDir    string
	CacheDir   string
	VMDir      string
	Config     string
	KeyPriv    string
	KeyPub     string
	RootDisk   string
	ZFSDisk    string
	SwapDisk   string
	EFIVars    string
	SeedISO    string
	QMPSock    string
	PIDFile    string
	ConsoleLog string
	QEMULog    string
}

// NewVMPaths computes the standard layout under baseDir (typically ~/.hotbed).
func NewVMPaths(baseDir string) VMPaths {
	vmDir := filepath.Join(baseDir, "vm")
	return VMPaths{
		BaseDir:    baseDir,
		CacheDir:   filepath.Join(baseDir, "cache"),
		VMDir:      vmDir,
		Config:     filepath.Join(vmDir, "config.json"),
		KeyPriv:    filepath.Join(vmDir, "id_ed25519"),
		KeyPub:     filepath.Join(vmDir, "id_ed25519.pub"),
		RootDisk:   filepath.Join(vmDir, "root.qcow2"),
		ZFSDisk:    filepath.Join(vmDir, "zfs.qcow2"),
		SwapDisk:   filepath.Join(vmDir, "swap.qcow2"),
		EFIVars:    filepath.Join(vmDir, "efivars.fd"),
		SeedISO:    filepath.Join(vmDir, "seed.iso"),
		QMPSock:    filepath.Join(vmDir, "qmp.sock"),
		PIDFile:    filepath.Join(vmDir, "qemu.pid"),
		ConsoleLog: filepath.Join(vmDir, "console.log"),
		QEMULog:    filepath.Join(vmDir, "qemu.log"),
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

// QEMUArgs builds the QEMU command-line arguments for cfg and paths.
// All writable disks use virtio-scsi-pci + scsi-hd with discard=unmap and
// detect-zeroes=unmap so guest TRIM/UNMAP (ZFS autotrim, zpool trim, fstrim,
// swapon --discard) punches holes out of the host qcow2 files on APFS.
func QEMUArgs(cfg VMConfig, p VMPaths, qmpSock string) ([]string, error) {
	if cfg.Arch != "arm64" && cfg.Arch != "amd64" {
		return nil, fmt.Errorf("unsupported VM architecture %q", cfg.Arch)
	}
	if cfg.CPUs <= 0 {
		return nil, fmt.Errorf("invalid VM CPU count %d", cfg.CPUs)
	}
	memMB := cfg.MemoryBytes / (1 << 20)
	if memMB < 512 {
		return nil, fmt.Errorf("VM memory %d MiB is too small (minimum 512 MiB)", memMB)
	}
	var args []string
	if cfg.Arch == "arm64" {
		args = append(args,
			"-machine", "virt,highmem=on",
			"-accel", "hvf",
			"-cpu", "host",
		)
	} else {
		args = append(args,
			"-machine", "q35",
			"-accel", "hvf",
			"-cpu", "host",
		)
	}
	args = append(args,
		"-name", "hotbed-"+cfg.Hostname,
		"-smp", strconv.Itoa(cfg.CPUs),
		"-m", strconv.FormatInt(memMB, 10),
		"-display", "none",
		"-monitor", "none",
		"-serial", "file:"+p.ConsoleLog,
		"-pidfile", p.PIDFile,
		// UEFI firmware + per-VM NVRAM variables.
		"-drive", "if=pflash,format=raw,readonly=on,file="+cfg.FirmwareCode,
		"-drive", "if=pflash,format=raw,file="+p.EFIVars,
		// VirtIO SCSI controller: rock-solid SCSI UNMAP -> qcow2 unmap -> APFS F_PUNCHHOLE.
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", "if=none,id=root,file="+p.RootDisk+",format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"-device", "scsi-hd,bus=scsi0.0,drive=root,serial=hotbed-root,bootindex=1",
		"-drive", "if=none,id=zfs,file="+p.ZFSDisk+",format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"-device", "scsi-hd,bus=scsi0.0,drive=zfs,serial=hotbed-zfs",
		"-drive", "if=none,id=swap,file="+p.SwapDisk+",format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"-device", "scsi-hd,bus=scsi0.0,drive=swap,serial=hotbed-swap",
		// Cloud-init NoCloud seed CD-ROM.
		"-drive", "if=none,id=seed,file="+p.SeedISO+",format=raw,readonly=on",
		"-device", "scsi-cd,bus=scsi0.0,drive=seed",
		// User-mode networking with localhost SSH forwarding (zero root needed).
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", cfg.SSHPort),
		"-device", "virtio-net-pci,netdev=net0",
		// Fast guest entropy.
		"-device", "virtio-rng-pci",
	)
	if qmpSock != "" {
		args = append(args, "-qmp", "unix:"+qmpSock+",server=on,wait=off")
	}
	return args, nil
}

// CloudInitMetaData renders the NoCloud meta-data YAML.
func CloudInitMetaData(hostname string) string {
	return fmt.Sprintf("instance-id: hotbed-%s\nlocal-hostname: %s\n", hostname, hostname)
}

// CloudInitUserData renders the NoCloud user-data YAML that boots SSH quickly
// with the generated management key and any host public keys.
func CloudInitUserData(hostname, username string, pubKeys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", hostname)
	b.WriteString("manage_etc_hosts: true\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("package_update: false\n")
	b.WriteString("package_upgrade: false\n")
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
// backing files / qcow2 images without initializing untouched metaslabs, and
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

// LaunchAgentPlist renders the macOS LaunchAgent plist that auto-starts the
// QEMU worker VM on login.
func LaunchAgentPlist(label, qemuBin string, args []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlEscape(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(qemuBin))
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <false/>\n")
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

// LinuxSetupMeta is persisted in <dir>/setup.json on Linux so status, start,
// stop, trim and destroy know how the worker was configured.
type LinuxSetupMeta struct {
	StateDir    string `json:"stateDir"`
	User        string `json:"user"`
	Controller  string `json:"controller"`
	StorageMode string `json:"storageMode"` // "btrfs", "zfs", "zfs-file"
	PoolName    string `json:"poolName,omitempty"`
	ImagePath   string `json:"imagePath,omitempty"`
	DiskSize    int64  `json:"diskSize,omitempty"`
	ServiceName string `json:"serviceName"`
	TrimTimer   string `json:"trimTimer,omitempty"`
}
