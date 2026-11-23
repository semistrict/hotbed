package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestPriorityFlagParsingAndLimits(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "prio-fox")
	c.workspace("prio-fox", nil)

	cases := []struct {
		flag string
		want int32
	}{
		{"", 0},
		{"normal", 0},
		{"low", -10},
		{"high", 10},
		{"urgent", 20},
		{"25", 25},
		{"-50", -50},
		{"100", 100},
		{"-100", -100},
	}
	for _, tc := range cases {
		args := []string{"run", "-s", "prio-fox", "-r", "app", "--detach"}
		if tc.flag != "" {
			if strings.HasPrefix(tc.flag, "-") {
				args = append(args, "--priority="+tc.flag)
			} else {
				args = append(args, "--priority", tc.flag)
			}
		}
		args = append(args, "--", "true")
		var cmd pb.Command
		c.ok(&cmd, args...)
		if got := cmd.GetLimits().GetPriority(); got != tc.want {
			t.Errorf("--priority %q -> Limits.Priority = %d, want %d", tc.flag, got, tc.want)
		}
	}

	for _, bad := range []string{"101", "-101", "super", "1.5"} {
		flag := "--priority=" + bad
		code, err := c.fails("run", "-s", "prio-fox", "-r", "app", "--detach", flag, "--", "true")
		if code != exitError || err.Code != api.CodeInvalidArgs {
			t.Errorf("--priority %q -> exit %d, code %q; want exit %d, %q", bad, code, err.Code, exitError, api.CodeInvalidArgs)
		}
	}
}

// TestPriorityPreemptionE2E verifies end-to-end priority preemption through the
// CLI, controller, and worker: a low-priority running command holding most of
// the node's memory is parked when a high-priority command runs, and thaws once
// the high-priority command finishes.
func TestPriorityPreemptionE2E(t *testing.T) {
	c := newCluster(t)
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "prio-low")
	c.ok(nil, "session", "start", "prio-high")
	c.workspace("prio-low", nil)
	c.workspace("prio-high", nil)

	// Start a low-priority command and have it hold 14 GiB of the 16 GiB node.
	var low pb.Command
	c.ok(&low, "run", "-s", "prio-low", "-r", "app", "--detach", "--priority", "low", "--", "sleep", "60")
	n := c.nodes[0]
	c.eventually("low-priority command to run", func() bool {
		cmd, ok := n.eng().Command(low.Id)
		return ok && cmd.State == pb.CommandState_COMMAND_STATE_RUNNING
	})
	n.units.SetAnon("hotbed-"+low.Id, 14<<30)
	c.eventually("worker to observe low-priority command's 14 GiB peak", func() bool {
		return n.eng().Heartbeat(c.id).ReservedMemoryBytes >= 14<<30
	})

	// Run a high-priority command via the CLI: it must preempt low, run, and exit 0.
	code, stdout, stderr := c.hotbed("run", "-s", "prio-high", "-r", "app", "--priority", "high", "--mem-low", "4G", "--", "sh", "-c", "echo urgent-ok; sleep 0.3")
	if code != 0 || !strings.Contains(stdout, "urgent-ok") {
		t.Fatalf("high-priority run: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// Verify low-priority command was parked (preempted) and then thawed back to running.
	c.eventually("low-priority command to thaw after high-priority finished", func() bool {
		cmd, ok := n.eng().Command(low.Id)
		if !ok || cmd.State != pb.CommandState_COMMAND_STATE_RUNNING {
			return false
		}
		var parkedPreempt, thawed bool
		for _, ev := range cmd.Timeline {
			if ev.What == "parked" && strings.Contains(ev.Detail, "preempted by higher-priority command") {
				parkedPreempt = true
			}
			if ev.What == "thawed" {
				thawed = true
			}
		}
		return parkedPreempt && thawed
	})

	// Verify ps shows the high-priority command succeeded with priority 10.
	var ls pb.ListCommandsResponse
	c.ok(&ls, "ps", "-s", "prio-high", "-e")
	if !slices.ContainsFunc(ls.Commands, func(cmd *pb.Command) bool {
		return cmd.State == pb.CommandState_COMMAND_STATE_SUCCEEDED && cmd.GetLimits().GetPriority() == 10
	}) {
		t.Errorf("expected succeeded priority-10 command in ps output: %+v", ls.Commands)
	}
}
