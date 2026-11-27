//go:build darwin

package worker

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/machinedtest"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
)

// TestLiveMachinedVMBackend runs a macOS worker's VM backend on a real
// machined: it provisions a base image in a VM of its own, versions it in
// machined's embedded cladefs daemon, forks a workspace from it, runs a
// command in the workspace's VM, and stops and deletes it.
func TestLiveMachinedVMBackend(t *testing.T) {
	m := machinedtest.Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	b, err := OpenVMBackend(ctx, filepath.Join(m.Dir, "worker"), statedir.VMConfig{
		Enabled:        true,
		MachinedSock:   m.Socket,
		MachinedImages: m.Images,
		KernelPath:     m.Kernel,
		GuestdBin:      m.GuestdBin,
		VCPUs:          2,
		MemMiB:         1024,
		DiskBytes:      8 << 30,
	}, "", "")
	if err != nil {
		t.Fatalf("OpenVMBackend: %v", err)
	}
	defer b.Close()

	base, err := b.EnsureProvisionedBaseImage(ctx, baseimage.Spec{
		Image:  "public.ecr.aws/debian/debian:trixie-slim",
		Arch:   "arm64",
		Script: "echo provisioned-on-machined > /etc/hotbed-provisioned",
	}, nil)
	if err != nil {
		t.Fatalf("EnsureProvisionedBaseImage: %v", err)
	}
	if _, err := b.PrepareWorkspace(ctx, "ws-live", base, 0, false); err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	rt := b.Runtime()
	if err := rt.StartCommand(ctx, "ws-live", runtime.CommandSpec{
		ID: "live", Argv: []string{"/bin/sh", "-c", "cat /etc/hotbed-provisioned; cat /etc/resolv.conf"}, Dir: "/work",
	}); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	var out bytes.Buffer
	if _, err := rt.StreamLogs(ctx, "ws-live", "live", 0, true, &out); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if !strings.Contains(out.String(), "provisioned-on-machined") || !strings.Contains(out.String(), "nameserver 10.0.2.1") {
		t.Fatalf("command output:\n%s\nwant the base's provisioning and machined's DNS", out.String())
	}
	if err := b.StopVM(ctx, "ws-live"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if err := b.DeleteWorkspace(ctx, "ws-live"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
}
