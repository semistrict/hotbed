package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/storage"
)

// A VM worker keeps nothing of its workspaces on its own filesystem: it
// opens no btrfs or ZFS store (it would wait forever on ext4 or xfs) and
// keeps no local base rootfs to pull and provision.
func TestVMWorkerOpensNoCopyOnWriteStoreOrBaseImage(t *testing.T) {
	dir := t.TempDir()
	cfg := statedir.WorkerConfig{
		VM: &statedir.VMConfig{Enabled: true},
		// Both enabled: VM wins, as Run's backend choice does.
		CladefsFUSE: &statedir.CladefsFUSEConfig{Enabled: true},
	}
	store, baseImg, err := openLocalRootfs(t.Context(), cfg, dir, baseimage.Spec{User: "dev", UID: 1001, GID: 1001},
		func(context.Context) (storage.Store, error) {
			t.Fatal("a VM worker opened a copy-on-write store")
			return nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Kind(); got != "none" {
		t.Fatalf("store kind = %q, want none", got)
	}
	if baseImg != nil {
		t.Fatalf("a VM worker keeps a local base image: %+v", baseImg)
	}
}

// A cladefs host-FUSE worker clones bases from a local base image and traces
// against its rootfs: it opens its copy-on-write store and keeps the image
// on it.
func TestFUSEWorkerKeepsBaseImageOnItsCopyOnWriteStore(t *testing.T) {
	cow := &fake.Store{Root: t.TempDir()}
	spec := baseimage.Spec{User: "dev", UID: 1001, GID: 1001}
	opened := false
	store, baseImg, err := openLocalRootfs(t.Context(), statedir.WorkerConfig{CladefsFUSE: &statedir.CladefsFUSEConfig{Enabled: true}}, t.TempDir(), spec,
		func(context.Context) (storage.Store, error) {
			opened = true
			return cow, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !opened || store != cow {
		t.Fatalf("store = %v (opened %v), want the copy-on-write store", store, opened)
	}
	if baseImg == nil || baseImg.Store != cow || baseImg.Spec != spec || baseImg.Provision == nil {
		t.Fatalf("base image = %+v, want spec %+v provisioned on the copy-on-write store", baseImg, spec)
	}
}

// What blocks a FUSE worker on its storage ends Run's wait with it.
func TestFUSEWorkerStorageErrorEndsOpen(t *testing.T) {
	want := errors.New("no pool")
	_, _, err := openLocalRootfs(t.Context(), statedir.WorkerConfig{CladefsFUSE: &statedir.CladefsFUSEConfig{Enabled: true}}, t.TempDir(), baseimage.Spec{},
		func(context.Context) (storage.Store, error) { return nil, want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
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
