package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A node gone for good is retired: it leaves hotbed nodes and placement,
// its session's next run goes elsewhere, and its history stays. A node
// still online is not retired, and one that joins again is a node again.
func TestRetireNodeGoneForGood(t *testing.T) {
	c := newFleet(t, 2)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "tidy-tern"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	t.Chdir(agent)

	node, _ := c.ranOn(S, "one\n", "--node", "mini", "--", "echo", "one")
	if node != "mini" {
		t.Fatalf("ran on %s, want mini", node)
	}
	// Online, it is refused.
	if _, e := c.fails("node", "retire", "mini"); e == nil || e.Code != api.CodeConflict || e.Message != "mini is online: only a node gone for good is retired" {
		t.Fatalf("retire an online node = %+v, want CONFLICT mini is online", e)
	}

	// Its machine is deleted.
	mini := c.nodes[1]
	mini.halt()
	c.clock.advance(api.OnlineWindow + time.Second)
	c.heartbeat() // asahimbp stays online by the new clock
	if code, out, stderr := c.hotbed("node", "retire", "mini"); code != 0 || out != "mini  retired\n" {
		t.Fatalf("node retire mini: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if code, out, stderr := c.hotbed("nodes"); code != 0 || out != "asahimbp          arm64   8 cpus  online\n" {
		t.Fatalf("nodes after retiring mini: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if _, e := c.fails("node", "show", "mini"); e == nil || e.Code != api.CodeNotFound {
		t.Fatalf("node show a retired node = %+v, want NOT_FOUND", e)
	}

	// The session's next run, which syncs its checkout, goes to the node left.
	if node, _ := c.ranOn(S, "two\n", "--", "echo", "two"); node != "asahimbp" {
		t.Fatalf("ran on %s after mini retired, want asahimbp", node)
	}

	// Joining again, it is a node again.
	joiner, _, err := api.Controller(strings.TrimPrefix(c.ctrlSrv.URL, "http://"), c.tn.Client(mini.peer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joiner.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := c.hotbed("nodes"); code != 0 || out != "asahimbp          arm64   8 cpus  online\nmini              arm64   8 cpus  offline\n" {
		t.Fatalf("nodes after mini joined again: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
}
