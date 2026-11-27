package worker

import (
	"context"
	"path/filepath"
	"testing"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/statedir"
)

func TestPickBase(t *testing.T) {
	p := &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
		Steps: []*pb.Step{{Name: "build", Hash: "h1"}},
	}
	const key = "key"
	base := func(cell uint64, recipe, baseKey string) *projectBase {
		return &projectBase{
			Ref:        cladefs.CellRef{Cell: cell, Version: 1},
			BaseID:     recipe,
			Recipe:     recipe,
			BaseKey:    baseKey,
			SetupHash:  projectSetupHash(p),
			Repos:      map[string]string{"app": "c0"},
			StepHashes: projectStepHashes(p),
		}
	}
	for _, tc := range []struct {
		name         string
		held, shared *projectBase
		fresh        bool
		want         uint64 // cell; 0 for none
	}{
		{"nothing", nil, nil, false, 0},
		{"only held", base(1, "old", key), nil, false, 1},
		{"only shared", nil, base(2, "old", key), false, 2},
		{"shared of another base image builds nothing on", nil, base(2, "old", "other"), false, 0},
		{"held up to date beats the controller's newer", base(1, "want", key), base(2, "old", key), false, 1},
		{"shared up to date beats a fresh held", base(1, "old", key), base(2, "want", key), true, 2},
		{"the controller's newest is built on", base(1, "old", key), base(2, "older", key), false, 2},
		{"a held base the controller has not heard of is built on", base(1, "old", key), base(2, "older", key), true, 1},
		{"shared beats a fresh held of another base image", base(1, "old", "other"), base(2, "older", key), true, 2},
		{"held beats a shared of another base image", base(1, "old", "other"), base(2, "older", "other"), false, 1},
		{"same cell", base(1, "old", key), base(1, "old", key), false, 1},
		{"unversioned shared is no base", base(1, "old", key), &projectBase{Ref: cladefs.CellRef{Cell: 2}}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickBase(tc.held, tc.shared, tc.fresh, p, key, "want")
			var cell uint64
			if got != nil {
				cell = got.Ref.Cell
			}
			if cell != tc.want {
				t.Fatalf("pickBase = cell %d, want %d", cell, tc.want)
			}
		})
	}
}

// sharingNode is a host-FUSE worker backend over a fake cladefsd, with a
// freshen runner that records each build it runs.
type sharingNode struct {
	b      *CladefsFUSEBackend
	api    *fakeRetireCladefsClient
	spec   baseimage.Spec
	builds []managedFreshenSpec
}

func newSharingNode(t *testing.T, firstCell uint64) *sharingNode {
	t.Helper()
	tmp := t.TempDir()
	api := &fakeRetireCladefsClient{nextCell: firstCell, nextVersion: firstCell}
	store, err := cladefs.NewStore(t.Context(), cladefs.Config{CellRoot: filepath.Join(tmp, "cells")}, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.SetMount(&fakeRetireMountClient{})
	b := newCladefsFUSEBackend(tmp, statedir.CladefsFUSEConfig{
		MountRoot:       filepath.Join(tmp, "mnt"),
		MountStoresRoot: filepath.Join(tmp, "mount-stores"),
	})
	b.clearNosuidFn = func(string) error { return nil }
	b.cleanupMount = func(string, bool) {}
	n := &sharingNode{
		b:    b,
		api:  api,
		spec: baseimage.Spec{Image: "debian:trixie-slim", Arch: "arm64"}.Normalized(),
	}
	n.b.store.Store(store)
	n.b.loadState()
	n.b.baseImages[n.b.baseKey(n.spec)] = cladefs.CellRef{Cell: firstCell - 1, Version: 1}
	return n
}

func (n *sharingNode) ensure(t *testing.T, p *pb.HostedProject, head string, shared *pb.SharedBase) *projectBase {
	t.Helper()
	run := func(_ context.Context, s managedFreshenSpec) (*managedFreshenHandle, error) {
		n.builds = append(n.builds, s)
		return &managedFreshenHandle{
			baseID: s.BaseID,
			cmdID:  "f-" + s.BaseID,
			stubResult: &managedFreshenResult{
				BaseID:    s.BaseID,
				CommandID: "f-" + s.BaseID,
				RepoHeads: s.Heads,
				State:     succeeded,
				Started:   true,
			},
		}, nil
	}
	res, err := n.b.ensureProjectBase(t.Context(), p, map[string]string{"app": head}, shared, n.spec, nil, nil, nil, run, ensureBaseOpts{})
	if err != nil {
		t.Fatalf("ensureProjectBase at %s: %v", head, err)
	}
	return res.Base
}

// heartbeat tells the controller what the node holds, as a heartbeat
// delivered does, and returns its one held base, as the controller would hand
// it to another node.
func (n *sharingNode) heartbeat(t *testing.T) *pb.SharedBase {
	t.Helper()
	_, _, held, retired := n.b.HeartbeatMeta()
	n.b.held.delivered(held, retired)
	if len(held) != 1 {
		t.Fatalf("held = %+v, want one base", held)
	}
	return held[0]
}

func demoProject(steps ...*pb.Step) *pb.HostedProject {
	return &pb.HostedProject{
		Name:  "demo",
		Repos: []*pb.Repo{{Id: "app", Url: "https://example.invalid/app.git", Branch: "main"}},
		Steps: steps,
	}
}

// A node with no base of its own builds on the base another node of its arch
// built, not from the base image, and the next node builds on that in turn:
// one chain of bases for the arch.
func TestNodesBuildOnEachOthersBases(t *testing.T) {
	p := demoProject(&pb.Step{Name: "build", Hash: "h1", EveryPull: true})
	a, b := newSharingNode(t, 100), newSharingNode(t, 200)

	first := a.ensure(t, p, "c1", nil)
	if len(a.builds) != 1 || !a.builds[0].Cold {
		t.Fatalf("a's builds = %+v, want one cold build", a.builds)
	}
	fromA := a.heartbeat(t)
	if fromA.GetCell().GetCell() != first.Ref.Cell || fromA.GetCell().GetRuntime() != runtime.KindCladefs || fromA.GetRepos()["app"] != "c1" {
		t.Fatalf("a holds %+v, want its base %d at c1", fromA, first.Ref.Cell)
	}

	// b, with no base of its own, builds c2 on a's base.
	second := b.ensure(t, p, "c2", fromA)
	if len(b.builds) != 1 || b.builds[0].Cold || b.builds[0].FromBaseID != first.BaseID {
		t.Fatalf("b's builds = %+v, want one built on %s", b.builds, first.BaseID)
	}
	if got := b.api.forkReqs[0].GetCell(); got != first.Ref.Cell {
		t.Fatalf("b forked cell %d, want a's base %d", got, first.Ref.Cell)
	}
	if got := b.builds[0].StepsToRun; len(got) != 1 || got[0].GetName() != "build" {
		t.Fatalf("b ran steps %+v, want only the every-pull build", got)
	}

	// a, holding its own older base, builds c3 on b's newer one.
	a.ensure(t, p, "c3", b.heartbeat(t))
	if len(a.builds) != 2 || a.builds[1].Cold || a.builds[1].FromBaseID != second.BaseID {
		t.Fatalf("a's second build = %+v, want one built on b's %s", a.builds[1], second.BaseID)
	}

	// b, asked again at c3, takes a's up-to-date base without building.
	fromA = a.heartbeat(t)
	third := b.ensure(t, p, "c3", fromA)
	if len(b.builds) != 1 || third.Ref.Cell != fromA.GetCell().GetCell() {
		t.Fatalf("b at c3 = cell %d after %d builds, want a's base without a build", third.Ref.Cell, len(b.builds))
	}
	if got := b.heartbeat(t).GetCell().GetCell(); got != third.Ref.Cell {
		t.Fatalf("b holds cell %d, want the base it hands out (%d)", got, third.Ref.Cell)
	}
}

// A node that just built a base builds on it while the controller, which has
// not heard of it yet, still hands out an older one.
func TestNodeBuildsOnItsOwnFreshBase(t *testing.T) {
	p := demoProject()
	a, b := newSharingNode(t, 100), newSharingNode(t, 200)
	a.ensure(t, p, "c1", nil)
	stale := a.heartbeat(t)
	mine := b.ensure(t, p, "c2", stale)
	b.ensure(t, p, "c3", stale)
	if len(b.builds) != 2 || b.builds[1].FromBaseID != mine.BaseID {
		t.Fatalf("b's second build = %+v, want one built on its own %s", b.builds[1], mine.BaseID)
	}
}

// A node with no base of its own hands out the cluster's base at once, and
// holds it, when a freshen to the new commit may wait for the background.
func TestNodeHandsOutSharedBaseWhileFreshenWaits(t *testing.T) {
	p := demoProject()
	a, b := newSharingNode(t, 100), newSharingNode(t, 200)
	a.ensure(t, p, "c1", nil)
	fromA := a.heartbeat(t)

	res, err := b.b.ensureProjectBase(t.Context(), p, map[string]string{"app": "c2"}, fromA, b.spec, nil, nil, nil, nil, ensureBaseOpts{
		Budget:          newFreshenBudget(nil, nil),
		AllowBackground: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base.Ref.Cell != fromA.GetCell().GetCell() || !res.DeferBackground {
		t.Fatalf("b handed out %+v (defer %v), want a's base %d and a background freshen", res.Base, res.DeferBackground, fromA.GetCell().GetCell())
	}
	if got := b.heartbeat(t).GetCell().GetCell(); got != fromA.GetCell().GetCell() {
		t.Fatalf("b holds cell %d, want a's base it hands out", got)
	}
}

// The base to build on is gone (retired while a stale copy of it was still to
// hand): a node builds on the base it holds, not from the base image.
func TestBuildOnHeldBaseWhenSharedBaseIsGone(t *testing.T) {
	p := demoProject()
	a, b := newSharingNode(t, 100), newSharingNode(t, 200)
	a.ensure(t, p, "c1", nil)
	mine := b.ensure(t, p, "c2", a.heartbeat(t))
	b.heartbeat(t)
	a.ensure(t, p, "c3", nil)
	gone := a.heartbeat(t)
	b.api.forkErr = func(req *cladefsdv1.ForkCellRequest) error {
		if req.GetCell() == gone.GetCell().GetCell() {
			return status.Error(codes.NotFound, "no such version")
		}
		return nil
	}

	b.ensure(t, p, "c4", gone)
	last := b.builds[len(b.builds)-1]
	if last.Cold || last.FromBaseID != mine.BaseID {
		t.Fatalf("b's build = %+v, want one built on its own %s", last, mine.BaseID)
	}
}

// A held base whose cell is gone is forgotten: the node builds from the base
// image, and no longer tells the controller it holds the gone one.
func TestHeldBaseWhoseCellIsGoneIsForgotten(t *testing.T) {
	p := demoProject()
	n := newSharingNode(t, 100)
	gone := n.ensure(t, p, "c1", nil)
	n.heartbeat(t)
	n.api.forkErr = func(req *cladefsdv1.ForkCellRequest) error {
		if req.GetCell() == gone.Ref.Cell {
			return status.Error(codes.NotFound, "no such version")
		}
		return nil
	}

	got := n.ensure(t, p, "c2", nil)
	if last := n.builds[len(n.builds)-1]; !last.Cold || last.ColdReason != "incremental_fork_failed" {
		t.Fatalf("build after the fork failed = %+v, want a cold one", last)
	}
	if held := n.heartbeat(t); held.GetCell().GetCell() != got.Ref.Cell {
		t.Fatalf("n holds %+v, want the new base %d", held, got.Ref.Cell)
	}
}

// The controller asks a node to retire base cells; the node deletes those it
// holds no longer, never one it holds, and reports each deletion until a
// heartbeat delivers it.
func TestHeldBasesRetireOnlyUnheldCells(t *testing.T) {
	ctx := t.Context()
	api := &fakeRetireCladefsClient{}
	store, err := cladefs.NewStore(ctx, cladefs.Config{CellRoot: filepath.Join(t.TempDir(), "cells")}, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var h heldBases
	h.hold("demo", &projectBase{Ref: cladefs.CellRef{Tenant: 3, Cell: 10, Version: 1}, BaseID: "demo-held"}, false, false)

	h.retire(ctx, store, []*pb.CladefsCell{
		{Tenant: 3, Cell: 10, BaseId: "demo-held"},
		{Tenant: 3, Cell: 11, BaseId: "demo-old"},
	})
	if len(api.deleteReqs) != 1 || api.deleteReqs[0].GetCell() != 11 {
		t.Fatalf("deleted %+v, want only cell 11", api.deleteReqs)
	}
	held, retired := h.report(runtime.KindVM)
	if len(retired) != 1 || retired[0].GetCell() != 11 {
		t.Fatalf("reported retired %+v, want cell 11", retired)
	}
	if _, again := h.report(runtime.KindVM); len(again) != 1 {
		t.Fatalf("an undelivered report of cell 11 went: %+v", again)
	}
	h.delivered(held, retired)
	if _, after := h.report(runtime.KindVM); len(after) != 0 {
		t.Fatalf("reported retired after delivery: %+v", after)
	}
}

// A base this node built and the controller has not heard of yet stays held
// against an older base picked before it was built; an up-to-date base, or
// any once the controller has heard of the fresh one, replaces it.
func TestHoldKeepsAFreshBaseAgainstOlderPicks(t *testing.T) {
	var h heldBases
	ref := func(cell uint64) *projectBase {
		return &projectBase{Ref: cladefs.CellRef{Cell: cell, Version: 1}, BaseID: "b"}
	}
	h.hold("demo", ref(1), false, false)
	h.hold("demo", ref(2), true, true)
	if got := h.hold("demo", ref(1), false, false); got.Ref.Cell != 2 {
		t.Fatalf("an older pick took the place of the fresh base: holds %d, want 2", got.Ref.Cell)
	}
	if got := h.hold("demo", ref(3), false, true); got.Ref.Cell != 3 {
		t.Fatalf("an up-to-date base did not replace the fresh one: holds %d, want 3", got.Ref.Cell)
	}
	h.hold("demo", ref(4), true, true)
	held, retired := h.report(runtime.KindVM)
	h.delivered(held, retired)
	if got := h.hold("demo", ref(5), false, false); got.Ref.Cell != 5 {
		t.Fatalf("a base the controller has heard of kept its place: holds %d, want 5", got.Ref.Cell)
	}
}
