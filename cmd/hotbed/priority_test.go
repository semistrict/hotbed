package main

import (
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestPriorityFlagParsingAndLimits(t *testing.T) {
	t.Parallel()
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
		args := []string{"run", "-s", "prio-fox", "-r", "app", "--detach", "--mem", "1G"}
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
		code, err := c.fails("run", "-s", "prio-fox", "-r", "app", "--detach", "--mem", "1G", flag, "--", "true")
		if code != exitError || err.Code != api.CodeInvalidArgs {
			t.Errorf("--priority %q -> exit %d, code %q; want exit %d, %q", bad, code, err.Code, exitError, api.CodeInvalidArgs)
		}
	}
}
