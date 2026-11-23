package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestValidateZFSDevicePath(t *testing.T) {
	valid := []string{
		"/dev/sdb",
		"/dev/nvme0n1",
		"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_hotbed-zfs",
		"/dev/disk/by-id/virtio-hotbed-zfs",
	}
	for _, dev := range valid {
		if err := validateZFSDevicePath(dev); err != nil {
			t.Errorf("validateZFSDevicePath(%q) = %v, want nil", dev, err)
		}
	}

	invalid := []string{
		"",
		"dev/sdb",
		"/dev",
		"/dev/",
		"/tmp/zfs.img",
		"/etc/passwd",
		"/dev/../etc/passwd",
		"/dev/disk/../sdb",
		"/dev//sdb",
		"/dev/sdb/",
		"/dev/sdb extra",
		"/dev/sdb\n",
	}
	for _, dev := range invalid {
		if err := validateZFSDevicePath(dev); err == nil {
			t.Errorf("validateZFSDevicePath(%q) = nil, want error", dev)
		}
	}
}

func TestCopyFileAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello-hotbed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFileAtomic(src, dst, 0o600); err != nil {
		t.Fatalf("copyFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello-hotbed" {
		t.Fatalf("dst content = %q, want hello-hotbed", string(got))
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("dst perm = %04o, want 0600", fi.Mode().Perm())
	}
	if err := copyFileAtomic(dir, filepath.Join(dir, "from-dir"), 0o600); err == nil {
		t.Fatal("copyFileAtomic on directory succeeded, want error")
	}
}
