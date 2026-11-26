// Package machinedtest starts a real machined for live tests on macOS: the
// daemon hotbed's macOS workers run their VMs on.
package machinedtest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Machined is a machined a test started, with what its VMs boot.
type Machined struct {
	// Socket is its API's Unix socket: Machined, and the Cladefsd service of
	// the cladefs daemon it embeds.
	Socket string
	// Images is its images directory; Kernel is cladefs's arm64 guest
	// kernel in it.
	Images, Kernel string
	// GuestdBin is a hotbed-guestd built for the guest (linux/arm64).
	GuestdBin string
	// Dir is the test's own directory, short enough for Unix sockets below
	// it, removed when the test ends.
	Dir string
}

// Start starts machined for t, or skips t unless HOTBED_LIVE_MACHINED names a
// machined binary (machined-vm beside it, both signed with the hypervisor
// entitlement) and HOTBED_LIVE_KERNEL_PATH cladefs's arm64 guest Image.
// machined is stopped when t ends, its log printed if t failed.
func Start(t *testing.T) *Machined {
	t.Helper()
	bin := os.Getenv("HOTBED_LIVE_MACHINED")
	kernelSrc := os.Getenv("HOTBED_LIVE_KERNEL_PATH")
	if bin == "" || kernelSrc == "" {
		t.Skip("set HOTBED_LIVE_MACHINED and HOTBED_LIVE_KERNEL_PATH to run live machined VMs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Short: machined's vsock sockets go under its state directory.
	dir, err := os.MkdirTemp("/tmp", "hbm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := &Machined{Socket: filepath.Join(dir, "machined.sock"), Images: filepath.Join(dir, "images"), Dir: dir}
	if err := os.MkdirAll(m.Images, 0o755); err != nil {
		t.Fatal(err)
	}
	m.Kernel = filepath.Join(m.Images, "Image")
	if err := os.Link(kernelSrc, m.Kernel); err != nil {
		t.Fatalf("link the kernel into machined's images: %v", err)
	}
	m.GuestdBin = filepath.Join(dir, "hotbed-guestd")
	build := exec.CommandContext(ctx, "go", "build", "-o", m.GuestdBin, "github.com/semistrict/hotbed/cmd/hotbed-guestd")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hotbed-guestd: %v\n%s", err, out)
	}

	daemon := exec.Command(bin, "--socket", m.Socket, "--images", m.Images, "--state", filepath.Join(dir, "state"))
	var log bytes.Buffer
	daemon.Stdout, daemon.Stderr = &log, &log
	if err := daemon.Start(); err != nil {
		t.Fatalf("start machined: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(os.Interrupt)
		<-exited
		if t.Failed() {
			t.Logf("machined's log:\n%s", log.String())
		}
	})
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(m.Socket); err == nil {
			return m
		}
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("machined exited before serving: %v\n%s", err, log.String())
		case <-ctx.Done():
			t.Fatalf("machined never served %s:\n%s", m.Socket, log.String())
		case <-tick.C:
		}
	}
}
