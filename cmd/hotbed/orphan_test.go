package main

import (
	"slices"
	"strings"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// When a worker loses worker.db and restarts, its startup orphan sweeper stops
// orphaned running units and removes orphaned workspace volumes, base volumes,
// and base snapshots before accepting new work, so the repo's next base builds
// cleanly (board 7923).
func TestOrphanSweepAfterLostWorkerDatabase(t *testing.T) {
	ctx := t.Context()
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "neat-fox")
	first := c.workspace("neat-fox", nil)

	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", "neat-fox", "-r", "app", "--detach", "--mem", "1G", "--", "sleep", "60")
	c.eventually("detached command to run", func() bool {
		return c.commandStates("neat-fox")[cmd.Id] == pb.CommandState_COMMAND_STATE_RUNNING
	})

	nd := c.nodes[0]
	// Sanity check: workspace volume, base volume, base snapshot, and running unit exist before wipe.
	preVols, err := nd.store.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(preVols, "ws/"+first.Id) || !slices.Contains(preVols, "bases/app/g1") {
		t.Fatalf("pre-wipe volumes = %q, want ws/%s and bases/app/g1", preVols, first.Id)
	}
	preSnaps, err := nd.store.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(preSnaps, "app-g1") {
		t.Fatalf("pre-wipe snapshots = %q, want app-g1", preSnaps)
	}
	preUnits, err := nd.units.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(preUnits, "hotbed-"+cmd.Id) {
		t.Fatalf("pre-wipe units = %q, want hotbed-%s", preUnits, cmd.Id)
	}

	// Wipe worker.db and restart the worker. Startup sweepOrphans must clear
	// every orphaned unit, volume, and snapshot immediately.
	nd.wipe()

	postUnits, err := nd.units.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(postUnits) != 0 {
		t.Errorf("orphan units survived DB wipe: %q", postUnits)
	}
	if st, err := nd.units.Status(ctx, "hotbed-"+cmd.Id); err != nil || st.Loaded || st.Running {
		t.Errorf("orphaned command unit still loaded/running after wipe: %+v, err=%v", st, err)
	}

	postVols, err := nd.store.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range postVols {
		if strings.HasPrefix(v, "ws/") || strings.HasPrefix(v, "bases/") {
			t.Errorf("orphan volume survived DB wipe: %q (all=%q)", v, postVols)
		}
	}

	postSnaps, err := nd.store.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range postSnaps {
		if !strings.HasPrefix(s, "baseimg-") {
			t.Errorf("orphan snapshot survived DB wipe: %q (all=%q)", s, postSnaps)
		}
	}

	// Wait for controller convergence, then verify a fresh workspace builds a
	// new base cleanly and runs commands against the repo.
	c.eventually("the loss to reach the session", func() bool {
		return len(ends(c.events("neat-fox"), cmd.Id)) > 0
	})
	c.converged()

	again := c.workspace("neat-fox", nil)
	if again.Id == first.Id {
		t.Fatalf("new workspace reused lost ID %s", first.Id)
	}
	if code, out, stderr := c.hotbed("run", "-s", "neat-fox", "-r", "app", "--", "cat", "src/lib.txt"); code != 0 || out != "base\n" {
		t.Errorf("run in fresh workspace after orphan sweep: exit %d, out=%q, stderr=%q", code, out, stderr)
	}
}
