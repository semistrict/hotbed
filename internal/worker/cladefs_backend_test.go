package worker

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
)

// newTestCladefsCore is the core of a backend of kind, keeping its state in
// dir and reaching cladefsd through api (and its host FUSE mount through a
// fake, for the host-FUSE backend).
func newTestCladefsCore(t *testing.T, kind, dir string, api cladefsdv1.CladefsdClient) *cladefsCore {
	t.Helper()
	store, err := cladefs.NewStore(t.Context(), cladefs.Config{CellRoot: filepath.Join(dir, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var core *cladefsCore
	switch kind {
	case runtime.KindVM:
		b := newVMBackend(dir, statedir.VMConfig{})
		core = &b.cladefsCore
	case runtime.KindCladefs:
		store.SetMount(&fakeRetireMountClient{nextCell: 800})
		b := newCladefsFUSEBackend(dir, statedir.CladefsFUSEConfig{
			MountRoot:       filepath.Join(dir, "mnt"),
			MountStoresRoot: filepath.Join(dir, "mount-stores"),
		})
		b.clearNosuidFn = func(string) error { return nil }
		b.cleanupMount = func(string, bool) {}
		core = &b.cladefsCore
	default:
		t.Fatalf("no cladefs backend of kind %q", kind)
	}
	core.store.Store(store)
	return core
}

// succeedingFreshen runs every base build's freshen to success at once.
func succeedingFreshen(_ context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
	return &managedFreshenHandle{
		baseID: s.BaseID,
		cmdID:  "f-" + s.BaseID,
		stubResult: &managedFreshenResult{
			BaseID: s.BaseID, CommandID: "f-" + s.BaseID, RepoHeads: s.Heads, State: succeeded, Started: true,
		},
	}, nil
}

// A build's cell can still be busy right after what built on it let it go
// (a VM's virtio-extent session, a FUSE mount): both backends retry making
// its version rather than throw a finished build away. The host-FUSE one
// once did not.
func TestProjectBaseBuildRetriesABusyMakeVersion(t *testing.T) {
	for _, kind := range []string{runtime.KindVM, runtime.KindCladefs} {
		t.Run(kind, func(t *testing.T) {
			busy := 2
			api := &fakeRetireCladefsClient{nextCell: 700, nextVersion: 10, makeVersionErr: func(*cladefsdv1.MakeVersionRequest) error {
				if busy > 0 {
					busy--
					return syscall.EBUSY
				}
				return nil
			}}
			core := newTestCladefsCore(t, kind, t.TempDir(), api)
			spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: goruntime.GOARCH}.Normalized()
			core.baseImages[core.peer.baseKey(spec)] = cladefs.CellRef{Cell: 600, Version: 1}
			proj := &pb.HostedProject{Name: "demo", Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}}}

			base, err := core.EnsureProjectBase(t.Context(), proj, map[string]string{"app": "abc123"}, nil, spec, nil, nil, nil, succeedingFreshen)
			if err != nil {
				t.Fatalf("EnsureProjectBase: %v", err)
			}
			if busy != 0 || len(api.makeVersionReqs) != 3 {
				t.Fatalf("MakeVersion asked %d times (%d busy answers left), want 3: twice busy, then made", len(api.makeVersionReqs), busy)
			}
			if base.Ref.Version != 11 || base.Ref.Cell == 0 {
				t.Fatalf("base %+v, want the cell built on at version 11", base.Ref)
			}
		})
	}
}

// Without a freshen runner, neither backend builds a base: there is no
// other way to run a build's freshen (the VM backend once had one, never
// reached).
func TestProjectBaseBuildNeedsAFreshenRunner(t *testing.T) {
	for _, kind := range []string{runtime.KindVM, runtime.KindCladefs} {
		t.Run(kind, func(t *testing.T) {
			api := &fakeRetireCladefsClient{nextCell: 700, nextVersion: 10}
			core := newTestCladefsCore(t, kind, t.TempDir(), api)
			spec := baseimage.Spec{Image: "debian:trixie-slim", Arch: goruntime.GOARCH}.Normalized()
			core.baseImages[core.peer.baseKey(spec)] = cladefs.CellRef{Cell: 600, Version: 1}
			proj := &pb.HostedProject{Name: "demo", Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}}}

			if _, err := core.EnsureProjectBase(t.Context(), proj, map[string]string{"app": "abc123"}, nil, spec, nil, nil, nil, nil); err == nil {
				t.Fatal("EnsureProjectBase built a base without a freshen runner")
			}
			if len(api.forkReqs) != 0 {
				t.Fatalf("forked %d cells for a build that cannot run, want none", len(api.forkReqs))
			}
		})
	}
}
