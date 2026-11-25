package setup

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVfkitClient(t *testing.T) {
	// Keep socket path short for macOS 104-byte Unix socket limit.
	tmpDir, err := os.MkdirTemp("/tmp", "hb-vfkit-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	sock := filepath.Join(tmpDir, "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Replies as vfkit 0.6.4 gives them.
	changes := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vm/state":
			_, _ = w.Write([]byte(`{"canHardStop":true,"canPause":true,"canResume":false,"canStart":false,"canStop":true,"state":"VirtualMachineStateRunning"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/vm/state":
			var req struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.State == "Pause" {
				http.Error(w, "VM cannot be paused", http.StatusBadRequest)
				return
			}
			changes <- req.State
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st, err := vfkitVMState(ctx, sock)
	if err != nil {
		t.Fatalf("vfkitVMState: %v", err)
	}
	if st != vfkitStateRunning {
		t.Fatalf("state = %q, want %q", st, vfkitStateRunning)
	}
	if err := vfkitSetVMState(ctx, sock, vfkitStop); err != nil {
		t.Fatalf("vfkitSetVMState(Stop): %v", err)
	}
	if got := <-changes; got != vfkitStop {
		t.Fatalf("vfkit got state change %q, want %q", got, vfkitStop)
	}
	err = vfkitSetVMState(ctx, sock, "Pause")
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request: VM cannot be paused") {
		t.Fatalf("vfkitSetVMState(Pause) = %v, want the 400 and its message", err)
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
