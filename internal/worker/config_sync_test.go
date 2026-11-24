package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/update"
	"github.com/semistrict/hotbed/internal/vm"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestVMBaseKeyInvalidatesOnGuestdOrConfigChange(t *testing.T) {
	dir := t.TempDir()
	guestd := filepath.Join(dir, "hotbed-guestd")
	if err := os.WriteFile(guestd, []byte("guestd-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &VMBackend{
		workerDir: dir,
		cfg: statedir.VMConfig{
			GuestdBin:  guestd,
			DiskBytes:  64 << 30,
			DNSServers: []string{"1.1.1.1"},
		},
		baseImages: map[string]cladefs.CellRef{},
	}
	spec := baseimage.Spec{User: "ramon", UID: 1000, GID: 1000}
	proj := &pb.HostedProject{Name: "cladefs"}

	key1 := b.vmBaseKey(spec)
	recipe1 := vmProjectRecipe(key1, proj, nil)

	// Changing hotbed-guestd bytes must change both vmBaseKey and vmProjectRecipe.
	if err := os.WriteFile(guestd, []byte("guestd-v2-with-dev-perms"), 0o755); err != nil {
		t.Fatal(err)
	}
	key2 := b.vmBaseKey(spec)
	recipe2 := vmProjectRecipe(key2, proj, nil)
	if key2 == key1 {
		t.Fatalf("vmBaseKey did not change when hotbed-guestd changed: %q", key1)
	}
	if recipe2 == recipe1 {
		t.Fatalf("vmProjectRecipe did not change when hotbed-guestd changed: %q", recipe1)
	}

	// Changing DNSServers via UpdateConfig must also change vmBaseKey.
	cfg2 := b.configSnapshot()
	cfg2.DNSServers = []string{"8.8.8.8"}
	b.UpdateConfig(cfg2)
	key3 := b.vmBaseKey(spec)
	if key3 == key2 {
		t.Fatalf("vmBaseKey did not change when DNSServers changed: %q", key2)
	}

	// loadCachedBases must index multiple baseimage-*.json files by exact key.
	cacheDir := filepath.Join(dir, "vm-bases")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, cell := range map[string]uint64{
		key2:           42,
		"zzzz-old-key": 99,
	} {
		raw, _ := json.Marshal(cladefs.CellRef{RelPath: "baseimages/vm-" + k, Tenant: 1, Cell: cell, Version: 1})
		if err := os.WriteFile(filepath.Join(cacheDir, "baseimage-"+k+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.baseImages = map[string]cladefs.CellRef{}
	b.loadCachedBases()
	if got := b.baseImages[key2]; got.Cell != 42 {
		t.Fatalf("loaded baseImages[%q] = %+v, want cell 42", key2, got)
	}
}

func TestArtifactStoreEnsureAndVerify(t *testing.T) {
	var hits atomic.Int32
	payload := []byte("fake-vmlinux-with-nested-kvm-enabled")
	payloadSHA := sha256Hex(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/vmlinux":
			_, _ = w.Write(payload)
		case "/corrupt":
			_, _ = w.Write([]byte("corrupted"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	workerDir := filepath.Join(t.TempDir(), "worker")
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &ArtifactStore{
		WorkerDir:      workerDir,
		DownloadClient: srv.Client(),
	}

	art := &pb.ManagedArtifact{
		Url:     srv.URL + "/vmlinux",
		Sha256:  payloadSHA,
		Version: "cladefs-6.12-kvm",
	}
	p1, err := store.Ensure(context.Background(), artifactKernel, "amd64", art)
	if err != nil {
		t.Fatalf("Ensure kernel: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("downloads = %d, want 1", hits.Load())
	}
	got, err := os.ReadFile(p1)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("read cached kernel = %q (%v), want %q", got, err, payload)
	}
	// Worker dir must be traversable (0111) so VMUser can open vmlinux.
	info, err := os.Stat(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Errorf("workerDir perm = %04o, want execute bits set for VMUser traversal", info.Mode().Perm())
	}
	// Second Ensure must hit cache with zero HTTP downloads.
	p2, err := store.Ensure(context.Background(), artifactKernel, "amd64", art)
	if err != nil || p2 != p1 {
		t.Fatalf("second Ensure = (%q, %v), want (%q, nil)", p2, err, p1)
	}
	if hits.Load() != 1 {
		t.Fatalf("downloads after cache hit = %d, want 1", hits.Load())
	}

	// Corrupted payload must fail SHA-256 verification.
	badArt := &pb.ManagedArtifact{
		Url:    srv.URL + "/corrupt",
		Sha256: sha256Hex([]byte("expected-other-bytes")),
	}
	if _, err := store.Ensure(context.Background(), artifactKernel, "amd64", badArt); err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
}

func TestMergeWorkerConfigAndRequiresRestart(t *testing.T) {
	local := statedir.WorkerConfig{
		ClusterID: "c1",
		User:      "ramon",
		Slice:     "user-1000-hotbed.slice",
		VM: &statedir.VMConfig{
			Enabled:        true,
			Default:        false,
			KernelPath:     "/var/lib/cladefs/vmlinux",
			GuestdBin:      "/usr/local/bin/hotbed-guestd",
			VCPUs:          4,
			MemMiB:         4096,
			DAXWindowBytes: 8 << 30,
			CacheType:      "Unsafe",
		},
	}

	remote := &pb.WorkerConfig{
		DefaultRuntime: proto.String(runtime.KindVM),
		Vm: &pb.WorkerVMConfig{
			Vcpus:          proto.Int32(8),
			MemMib:         proto.Int32(12288),
			DaxWindowBytes: proto.Uint64(16 << 30),
		},
	}
	merged := MergeWorkerConfig(local, remote)
	if !merged.VM.Default || merged.VM.VCPUs != 8 || merged.VM.MemMiB != 12288 || merged.VM.DAXWindowBytes != 16<<30 {
		t.Fatalf("merged VM = %+v", merged.VM)
	}
	if merged.VM.KernelPath != "/var/lib/cladefs/vmlinux" {
		t.Fatalf("unset kernelPath was overwritten: %q", merged.VM.KernelPath)
	}
	if RequiresConfigRestart(local, merged) {
		t.Fatal("hot-reloadable VM tuning change should NOT require restart")
	}

	// Structural change (slice or vm.enabled toggle) MUST require restart.
	structural := MergeWorkerConfig(local, &pb.WorkerConfig{
		Slice: proto.String("hotbed.slice"),
	})
	if !RequiresConfigRestart(local, structural) {
		t.Fatal("slice change must require restart")
	}
	vmDisabled := MergeWorkerConfig(local, &pb.WorkerConfig{
		Vm: &pb.WorkerVMConfig{Enabled: proto.Bool(false)},
	})
	if !RequiresConfigRestart(local, vmDisabled) {
		t.Fatal("toggling vm.enabled must require restart")
	}
}

func TestConfigSyncerHotReloadsManagedArtifactsAndRestartsOnStructuralChange(t *testing.T) {
	kernelBytes := []byte("nested-kvm-vmlinux-v2")
	guestdBytes := []byte("hotbed-guestd-with-dev-0666")
	kernelSHA := sha256Hex(kernelBytes)
	guestdSHA := sha256Hex(guestdBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vmlinux":
			_, _ = w.Write(kernelBytes)
		case "/guestd":
			_, _ = w.Write(guestdBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	workerDir := t.TempDir()
	customGuestd := filepath.Join(workerDir, "initial-guestd")
	if err := os.WriteFile(customGuestd, []byte("initial-guestd"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := statedir.WorkerConfig{
		ClusterID: "c1",
		User:      "ramon",
		VM: &statedir.VMConfig{
			Enabled:    true,
			Default:    true,
			KernelPath: "/var/lib/cladefs/vmlinux",
			GuestdBin:  customGuestd,
			VCPUs:      4,
			MemMiB:     4096,
		},
	}

	mgr, err := vm.NewManager(vm.Config{
		RunDir:         filepath.Join(workerDir, "vms"),
		KernelPath:     local.VM.KernelPath,
		GuestdBin:      local.VM.GuestdBin,
		DefaultVCPUs:   4,
		DefaultMemMiB:  4096,
		DisableNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	vmb := &VMBackend{
		cfg:        *local.VM,
		workerDir:  workerDir,
		mgr:        mgr,
		baseImages: map[string]cladefs.CellRef{},
	}
	eng := &Engine{cfg: EngineConfig{VM: vmb}}
	artifacts := &ArtifactStore{WorkerDir: workerDir, DownloadClient: srv.Client()}
	syncer := NewConfigSyncer(workerDir, local, nil, local, artifacts)
	syncer.Arch = "amd64"

	// 1. Hot-reloadable update: new vCPUs, managed kernel, and Build.guestd.
	reply := &pb.HeartbeatResponse{
		Build: &pb.Build{
			Number: 178,
			Guestd: &pb.ManagedArtifact{
				Url:     srv.URL + "/guestd",
				Sha256:  guestdSHA,
				Version: "build-178",
			},
		},
		WorkerConfig: &pb.WorkerConfig{
			Revision: 1,
			Vm: &pb.WorkerVMConfig{
				Vcpus:  proto.Int32(8),
				MemMib: proto.Int32(12288),
				Kernels: map[string]*pb.ManagedArtifact{
					"amd64": {
						Url:     srv.URL + "/vmlinux",
						Sha256:  kernelSHA,
						Version: "cladefs-6.12-kvm",
					},
				},
			},
		},
	}
	if needsRestart := syncer.Reconcile(context.Background(), eng, reply); needsRestart {
		t.Fatal("hot-reloadable config + artifacts should not require restart")
	}

	// Verify VMBackend and vm.Manager received the new kernel, guestd, and vCPUs live.
	fc := mgr.BuildFirecrackerConfig(vm.LaunchSpec{
		ID:            "ws-1",
		CellHostPath:  "/cell",
		CladefsVMSock: "/run/vm.sock",
	}, 3, "/vsock.sock", vm.NetAlloc{})
	if fc.MachineConfig.VCPUCount != 8 || fc.MachineConfig.MemSizeMiB != 12288 {
		t.Fatalf("Firecracker machine config = %+v, want 8 vcpus / 12288 MiB", fc.MachineConfig)
	}
	gotKernel, err := os.ReadFile(fc.BootSource.KernelImagePath)
	if err != nil || string(gotKernel) != string(kernelBytes) {
		t.Fatalf("Firecracker kernel %q = %q (%v), want %q", fc.BootSource.KernelImagePath, gotKernel, err, kernelBytes)
	}
	gotGuestd, err := os.ReadFile(vmb.configSnapshot().GuestdBin)
	if err != nil || string(gotGuestd) != string(guestdBytes) {
		t.Fatalf("VMBackend guestd %q = %q (%v), want %q", vmb.configSnapshot().GuestdBin, gotGuestd, err, guestdBytes)
	}

	// Verify controller-config.json was persisted on disk.
	loaded, err := LoadControllerConfig(workerDir)
	if err != nil || loaded == nil || loaded.GetRevision() != 1 {
		t.Fatalf("LoadControllerConfig = (%+v, %v), want rev 1", loaded, err)
	}

	// 2. Structural change (disabling VM) must request restart.
	reply2 := &pb.HeartbeatResponse{
		WorkerConfig: &pb.WorkerConfig{
			Revision: 2,
			Vm:       &pb.WorkerVMConfig{Enabled: proto.Bool(false)},
		},
	}
	if needsRestart := syncer.Reconcile(context.Background(), eng, reply2); !needsRestart {
		t.Fatal("disabling VM must request worker restart")
	}

	// 3. Follow.restartWhenIdle returns update.ErrRestart when idle.
	f := &Follow{}
	if err := f.restartWhenIdle(nil, "config changed"); !errors.Is(err, update.ErrRestart) {
		t.Fatalf("restartWhenIdle = %v, want update.ErrRestart", err)
	}
}
