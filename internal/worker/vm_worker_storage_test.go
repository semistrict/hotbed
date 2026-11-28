package worker

import (
	"errors"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
)

// Workers require the VM + cladefs runtime: a config without vm.enabled is not
// runnable even if legacy cladefs_fuse is enabled.
func TestRuntimeEnabledRequiresVM(t *testing.T) {
	if runtimeEnabled(statedir.WorkerConfig{}) {
		t.Fatal("empty config should not enable a runtime")
	}
	if runtimeEnabled(statedir.WorkerConfig{CladefsFUSE: &statedir.CladefsFUSEConfig{Enabled: true}}) {
		t.Fatal("cladefs_fuse alone must not enable a runtime; workers require VMs + cladefs")
	}
	if !runtimeEnabled(statedir.WorkerConfig{VM: &statedir.VMConfig{Enabled: true}}) {
		t.Fatal("vm.enabled=true should enable the runtime")
	}
}

// An engine with a VM backend runs on a store of no volumes in a plain
// directory, without a local base image: its rootfs identity is BaseSpec,
// and a chroot base, which only a local base image could start, fails hard.
func TestVMEngineRunsWithoutLocalBaseImage(t *testing.T) {
	dir := t.TempDir()
	spec := baseimage.Spec{User: "dev", UID: 1001, GID: 1002}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n", Host: "ubu1",
		Store: storage.NewDir(dir), BaseSpec: spec,
		Units: &fake.Units{}, System: fake.NewSystem(),
		VM: newVMBackend(dir, statedir.VMConfig{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if got, want := eng.baseImageKey(), spec.Key(); got != want {
		t.Fatalf("base image key = %s, want %s", got, want)
	}
	if got, want := eng.chrootHome(), spec.Normalized().Home; got != want {
		t.Fatalf("chroot home = %s, want %s", got, want)
	}
	p := &pb.HostedProject{Name: "proj"}
	if _, err := eng.startFreshen(t.Context(), p, 1, nil, "no_previous_base", heads{}, "r"); !errors.Is(err, errNoLocalBaseImage) {
		t.Fatalf("cold chroot base on a VM worker: err = %v, want errNoLocalBaseImage", err)
	}
}

// BaseSpec comes from BaseImage when only it is set, and an engine given
// both, differing, refuses to guess which one its rootfs is.
func TestNewEngineBaseSpecFollowsBaseImage(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	img := fake.BaseImage(store)
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1",
		Store: store, BaseImage: img, Units: &fake.Units{}, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if eng.cfg.BaseSpec != img.Spec {
		t.Fatalf("BaseSpec = %+v, want BaseImage's %+v", eng.cfg.BaseSpec, img.Spec)
	}
	other := img.Spec
	other.UID++
	if _, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1",
		Store: store, BaseImage: img, BaseSpec: other, Units: &fake.Units{}, System: fake.NewSystem(),
	}); err == nil {
		t.Fatal("NewEngine accepted a BaseSpec differing from its BaseImage's Spec")
	}
}
