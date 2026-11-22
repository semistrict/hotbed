package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"256G", 256 << 30},
		{"8GiB", 8 << 30},
		{"32gb", 32 << 30},
		{"512M", 512 << 20},
		{"1024MiB", 1024 << 20},
		{"1T", 1 << 40},
		{"64K", 64 << 10},
		{"4096", 4096},
		{"4096B", 4096},
	}
	for _, tc := range cases {
		got, err := ParseBytes(tc.in)
		if err != nil {
			t.Errorf("ParseBytes(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "0", "0G", "-4G", "abc", "9999999999T"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) succeeded, want error", bad)
		}
	}
}

func TestSanitizeHostname(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Ramons-MacBook-Pro.local", "ramons-macbook-pro"},
		{"My_Host 01!", "my-host-01"},
		{"---", "hotbed-worker"},
		{"", "hotbed-worker"},
		{"m4max", "m4max"},
	}
	for _, tc := range cases {
		if got := SanitizeHostname(tc.in); got != tc.want {
			t.Errorf("SanitizeHostname(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUbuntuCloudImageURLAndSHA256Sums(t *testing.T) {
	url, file, sums := UbuntuCloudImageURL("26.04", "arm64")
	if file != "ubuntu-26.04-server-cloudimg-arm64.img" {
		t.Fatalf("filename = %q", file)
	}
	if url != "https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-arm64.img" {
		t.Fatalf("url = %q", url)
	}
	if sums != "https://cloud-images.ubuntu.com/releases/26.04/release/SHA256SUMS" {
		t.Fatalf("sums = %q", sums)
	}

	sample := strings.Join([]string{
		"aaaa1111 *ubuntu-26.04-server-cloudimg-amd64.img",
		"BBBB2222 *ubuntu-26.04-server-cloudimg-arm64.img",
	}, "\n")
	got, ok := ParseSHA256Sums(sample, file)
	if !ok || got != "bbbb2222" {
		t.Fatalf("ParseSHA256Sums = (%q, %v), want (bbbb2222, true)", got, ok)
	}
	if _, ok := ParseSHA256Sums(sample, "missing.img"); ok {
		t.Fatalf("ParseSHA256Sums matched missing file")
	}
}

func TestQEMUArgs(t *testing.T) {
	p := NewVMPaths("/Users/test/.hotbed")
	cfg := VMConfig{
		Hostname:     "mac-worker",
		Arch:         "arm64",
		CPUs:         4,
		MemoryBytes:  8 << 30,
		SSHPort:      22222,
		FirmwareCode: "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
	}
	args, err := QEMUArgs(cfg, p, p.QMPSock)
	if err != nil {
		t.Fatalf("QEMUArgs arm64: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-machine virt,highmem=on",
		"-accel hvf",
		"-cpu host",
		"-smp 4",
		"-m 8192",
		"-device virtio-scsi-pci,id=scsi0",
		"id=root,file=/Users/test/.hotbed/vm/root.qcow2,format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"id=zfs,file=/Users/test/.hotbed/vm/zfs.qcow2,format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"serial=hotbed-zfs",
		"id=swap,file=/Users/test/.hotbed/vm/swap.qcow2,format=qcow2,discard=unmap,detect-zeroes=unmap,cache=writeback",
		"serial=hotbed-swap",
		"hostfwd=tcp:127.0.0.1:22222-:22",
		"-qmp unix:/Users/test/.hotbed/vm/qmp.sock,server=on,wait=off",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("QEMUArgs missing %q in:\n%s", want, joined)
		}
	}

	cfg.Arch = "amd64"
	argsAMD64, err := QEMUArgs(cfg, p, p.QMPSock)
	if err != nil {
		t.Fatalf("QEMUArgs amd64: %v", err)
	}
	if !slices.Contains(argsAMD64, "q35") {
		t.Errorf("amd64 args missing q35 machine: %v", argsAMD64)
	}

	cfg.Arch = "riscv64"
	if _, err := QEMUArgs(cfg, p, p.QMPSock); err == nil {
		t.Errorf("expected error for unsupported arch")
	}
}

func TestCloudInitTemplates(t *testing.T) {
	meta := CloudInitMetaData("mac-worker")
	if !strings.Contains(meta, "local-hostname: mac-worker") {
		t.Errorf("unexpected meta-data:\n%s", meta)
	}
	userData := CloudInitUserData("mac-worker", "ramon", []string{"ssh-ed25519 AAAA test@host"})
	for _, want := range []string{
		"#cloud-config",
		"hostname: mac-worker",
		"name: ramon",
		"NOPASSWD:ALL",
		"ssh-ed25519 AAAA test@host",
	} {
		if !strings.Contains(userData, want) {
			t.Errorf("user-data missing %q:\n%s", want, userData)
		}
	}
}

func TestSystemdUnitsAndLaunchAgent(t *testing.T) {
	u := SystemdWorkerUnit("/usr/local/bin/hotbed", "/home/ramon/.hotbed/worker", "/sbin/zpool import -d /home/ramon/.hotbed hotbed")
	for _, want := range []string{
		"ExecStartPre=-/sbin/zpool import -d /home/ramon/.hotbed hotbed",
		"ExecStart=/usr/local/bin/hotbed serve /home/ramon/.hotbed/worker",
		"After=network-online.target tailscaled.service zfs-mount.service",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("worker unit missing %q:\n%s", want, u)
		}
	}

	svc, tmr := SystemdTrimUnits("hotbed", true)
	if !strings.Contains(svc, "zpool wait -t free hotbed || true; zpool sync hotbed || true") || !strings.Contains(svc, "fstrim -a || true") {
		t.Errorf("trim service unexpected:\n%s", svc)
	}
	if !strings.Contains(tmr, "OnUnitActiveSec=1h") {
		t.Errorf("trim timer unexpected:\n%s", tmr)
	}

	plist := LaunchAgentPlist("com.semistrict.hotbed.worker", "/opt/homebrew/bin/qemu-system-aarch64", []string{"-m", "8192"}, "/tmp/qemu.log")
	for _, want := range []string{
		"<string>com.semistrict.hotbed.worker</string>",
		"<string>/opt/homebrew/bin/qemu-system-aarch64</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestQMPClient(t *testing.T) {
	// Keep socket path short for macOS 104-byte Unix socket limit.
	tmpDir, err := os.MkdirTemp("/tmp", "hb-qmp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	sock := filepath.Join(tmpDir, "q.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gotPowerdown := make(chan bool, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte(`{"QMP":{"version":{"qemu":{"major":11}}}}` + "\n"))
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req struct {
						Execute string `json:"execute"`
					}
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					switch req.Execute {
					case "qmp_capabilities":
						_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
					case "query-status":
						// Send an async event first to ensure the client ignores events.
						_, _ = c.Write([]byte(`{"event":"RESUME"}` + "\n"))
						_, _ = c.Write([]byte(`{"return":{"status":"running"}}` + "\n"))
					case "system_powerdown":
						gotPowerdown <- true
						_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
					}
				}
			}(conn)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st, err := QMPQueryStatus(ctx, sock)
	if err != nil {
		t.Fatalf("QMPQueryStatus: %v", err)
	}
	if st != "running" {
		t.Fatalf("status = %q, want running", st)
	}
	if err := QMPPowerdown(ctx, sock); err != nil {
		t.Fatalf("QMPPowerdown: %v", err)
	}
	select {
	case <-gotPowerdown:
	case <-time.After(time.Second):
		t.Fatal("mock QMP server did not receive system_powerdown")
	}
}

func TestFileDiskStatSparse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sparse.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	const virtual = 128 << 20 // 128 MiB
	if err := f.Truncate(virtual); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	ds, err := FileDiskStat("zfs", p, virtual)
	if err != nil {
		t.Fatal(err)
	}
	if ds.LogicalBytes != virtual {
		t.Errorf("LogicalBytes = %d, want %d", ds.LogicalBytes, virtual)
	}
	if ds.AllocatedBytes >= virtual {
		t.Errorf("AllocatedBytes = %d, expected sparse (< %d)", ds.AllocatedBytes, virtual)
	}
}
