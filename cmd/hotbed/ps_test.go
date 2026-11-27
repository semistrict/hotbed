package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// ps --ended finds the commands history keeps once their worker has
// forgotten them, any session's, and --match and --limit narrow what it
// finds, live commands too (board 7682).
func TestPsSearchesHistory(t *testing.T) {
	t.Parallel()
	workerClock := &clock{}
	c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.Now = workerClock.Now })
	c.addRepo(upstream(t))
	for _, s := range []string{"keen-elk", "calm-yak"} {
		c.ok(nil, "session", "start", s)
		c.workspace(s, nil)
	}
	finished := func(session, script string) string {
		t.Helper()
		cmd := &pb.Command{}
		c.ok(cmd, "run", "-s", session, "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", script)
		if code, _, stderr := c.hotbed("logs", "-f", "-s", session, cmd.Id); code != 0 {
			t.Fatalf("logs -f %s: exit %d (stderr %q)", script, code, stderr)
		}
		c.eventually("history to have "+script, func() bool {
			_, ok, err := c.store.Finished(t.Context(), cmd.Id)
			return err == nil && ok
		})
		return cmd.Id
	}
	one := finished("keen-elk", "echo alpha-one")
	beta := finished("keen-elk", "echo beta")
	two := finished("calm-yak", "echo alpha-two")
	workerClock.advance(3 * time.Minute)
	c.eventually("its worker to forget them", func() bool {
		return !slices.ContainsFunc([]string{one, beta, two}, func(id string) bool {
			_, ok := c.engine().Command(id)
			return ok
		})
	})
	live := &pb.Command{}
	c.ok(live, "run", "-s", "calm-yak", "-r", "app", "--detach", "--mem", "1G", "--", "sh", "-c", "sleep 30; echo alpha-live")
	t.Cleanup(func() { c.hotbed("stop", "-s", "calm-yak", live.Id) })

	ps := func(args ...string) ([]string, int32) {
		t.Helper()
		var ls pb.ListCommandsResponse
		c.ok(&ls, append([]string{"ps"}, args...)...)
		var ids []string
		for _, cmd := range ls.Commands {
			if cmd.Kind != "freshen" {
				ids = append(ids, cmd.Id)
			}
		}
		return ids, ls.More
	}
	for _, tc := range []struct {
		args     []string
		want     []string
		wantMore int32
	}{
		{[]string{"-p", "app"}, []string{live.Id}, 0},
		{[]string{"-p", "app", "--ended"}, []string{one, beta, two, live.Id}, 0},
		{[]string{"-p", "app", "-e", "--match", "alpha"}, []string{one, two, live.Id}, 0},
		{[]string{"-s", "keen-elk", "-e", "-m", "alpha"}, []string{one}, 0},
		{[]string{"--all", "-m", "alpha"}, []string{live.Id}, 0},
		// The limit counts ended commands: a running one always shows.
		{[]string{"--all", "-e", "-m", "alpha", "-n", "1"}, []string{two, live.Id}, 1},
		{[]string{"-p", "app", "-e", "-n", "1"}, []string{two, live.Id}, 2},
		{[]string{"-p", "app", "-e", "-n", "0"}, []string{one, beta, two, live.Id}, 0},
		{[]string{"--all", "-e", "-m", "no-such-text"}, nil, 0},
	} {
		if got, more := ps(tc.args...); !slices.Equal(got, tc.want) || more != tc.wantMore {
			t.Errorf("ps %v = %v (%d more), want %v (%d more)", tc.args, got, more, tc.want, tc.wantMore)
		}
	}

	// A command history recalls keeps its peak memory (fake units report
	// 64 MiB), not 0 as if it had used none.
	var ended pb.ListCommandsResponse
	c.ok(&ended, "ps", "-s", "keen-elk", "-e", "-m", "alpha")
	if len(ended.Commands) != 1 || ended.Commands[0].Id != one || ended.Commands[0].MemoryBytes != 64<<20 {
		t.Errorf("ps -e recalled %v, want only %s with MemoryBytes %d", ended.Commands, one, 64<<20)
	}

	// The plain listing says what the limit left out.
	code, stdout, stderr := c.hotbed("ps", "--all", "-e", "-m", "alpha", "-n", "1")
	if code != 0 || !strings.HasSuffix(stdout, "\n(+1 older ended, not shown: --limit 0 for every one)\n") || strings.Contains(stdout, one) {
		t.Errorf("ps -n 1: exit %d, stdout %q (stderr %q); want two rows and a line saying 1 more", code, stdout, stderr)
	}

	if _, err := c.fails("ps", "--all", "-n", "-1"); err == nil || err.Code != api.CodeInvalidArgs {
		t.Errorf("ps -n -1 = %v, want %s", err, api.CodeInvalidArgs)
	}
}
