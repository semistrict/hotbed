package setup

import (
	"fmt"
	"strconv"
	"strings"
)

// SystemdWorkerUnit renders the systemd unit file for hotbed-worker.service.
// When zfsImportPre is non-empty (file-backed ZFS pool on Linux), it runs as
// ExecStartPre so the pool imports before hotbed serve starts.
func SystemdWorkerUnit(binaryPath, stateDir, zfsImportPre string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=hotbed worker\n")
	b.WriteString("After=network-online.target tailscaled.service zfs-mount.service\n")
	// After= only orders: a host without tailscaled runs the worker all
	// the same (it needs no tailnet), so nothing here Wants it.
	b.WriteString("Wants=network-online.target zfs-mount.service\n\n")
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
// backing files / disk images without initializing untouched metaslabs.
func SystemdTrimUnits(poolName string) (service, timer string) {
	cmd := fmt.Sprintf("zpool wait -t free %[1]s || true; zpool sync %[1]s || true; zpool sync %[1]s || true; zpool sync %[1]s || true", poolName)
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

// launchAgentExitTimeout is how many seconds launchd gives hotbed's daemon
// to exit before it kills it: more than it takes to stop the Mac's worker
// and machined (macworker.Supervisor: 30 s, then 90 s).
const launchAgentExitTimeout = 150

// LaunchAgentPlist renders the macOS LaunchAgent plist that runs program
// with args from login on, and again whenever it exits: hotbed's daemon.
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
	// What it runs is in process groups of its own, which it stops itself
	// as it exits; launchd leaves them to it.
	b.WriteString("  <key>AbandonProcessGroup</key>\n  <true/>\n")
	// Told to exit while it stops the worker and machined, it finishes first.
	fmt.Fprintf(&b, "  <key>ExitTimeOut</key>\n  <integer>%d</integer>\n", launchAgentExitTimeout)
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
