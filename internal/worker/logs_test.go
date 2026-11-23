package worker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func mustLogPath(t *testing.T, e *Engine, id string) string {
	t.Helper()
	path, err := e.logPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// writeLogAt writes size bytes of command id's log, last written at.
func writeLogAt(t *testing.T, e *Engine, id string, size int, at time.Time) {
	t.Helper()
	path := mustLogPath(t, e, id)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func logsLeft(t *testing.T, e *Engine) []string {
	t.Helper()
	ents, err := os.ReadDir(e.logsDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		out = append(out, ent.Name())
	}
	return out
}

// Logs of forgotten commands go after their retention, and the oldest
// past the budget; a command the engine still has keeps its log however
// old, and a pruned log's reader hears why it is gone.
func TestPruneLogs(t *testing.T) {
	e, _ := outputEngine(t, cmdlog.Limit{})
	now := time.Now()
	writeLogAt(t, e, "expired", 10, now.Add(-8*24*time.Hour))
	if err := os.WriteFile(mustLogPath(t, e, "expired")+".dropped", []byte("1048576 2097152\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLogAt(t, e, "kept", 10, now.Add(-8*24*time.Hour))
	e.commands["kept"] = e.newCommand(&pb.CommandRecord{Command: &pb.Command{Id: "kept", State: running}})
	for name, at := range map[string]time.Time{
		"gone.log.dropped":       now,                     // its log is gone
		"newest.log.dropped.123": now,                     // being written
		"oldest.log.dropped.456": now.Add(-2 * time.Hour), // left half written
		".punch-check.789":       now.Add(-2 * time.Hour),
	} {
		path := filepath.Join(e.logsDir(), name)
		if err := os.WriteFile(path, []byte("1048576 2097152\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeLogAt(t, e, "oldest", 1<<20, now.Add(-3*time.Hour))
	writeLogAt(t, e, "older", 1<<20, now.Add(-2*time.Hour))
	writeLogAt(t, e, "newest", 1<<20, now.Add(-time.Hour))
	// 4 MiB long but compacted to half a MiB: it counts as what it keeps.
	writeLogAt(t, e, "compacted", 4<<20, now)
	if err := os.WriteFile(mustLogPath(t, e, "compacted")+".dropped", []byte("524288 4194304\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.pruneLogsTo(7*24*time.Hour, 2<<20+1<<19)
	want := []string{"compacted.log", "compacted.log.dropped", "kept.log", "newest.log", "newest.log.dropped.123", "older.log"}
	if got := logsLeft(t, e); !slices.Equal(got, want) {
		t.Errorf("logs left %q, want %q", got, want)
	}
	_, _, err := e.Follow("expired")
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound ||
		aerr.Message != "no log of command expired on this node: logs of ended commands are kept 7 days, and to 8.0 GiB in all" {
		t.Errorf("following a pruned log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.logsDir(), "expired.log.dropped")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pruned log's dropped record is left: %v", err)
	}
}

// A compacted log is budgeted by its kept bytes (excluding its punched hole),
// not its logical file size (board 7650).
func TestPruneLogsCountsCompactedLogByKeptBytes(t *testing.T) {
	e, _ := outputEngine(t, cmdlog.Limit{})
	now := time.Now()
	writeLogAt(t, e, "compacted", 10<<20, now.Add(-2*time.Hour))
	compactedPath := mustLogPath(t, e, "compacted")
	if _, err := cmdlog.Compact(compactedPath, cmdlog.Limit{Head: 1 << 20, Tail: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	// Restore mtime after Compact so ordering against newest is deterministic.
	if err := os.Chtimes(compactedPath, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	writeLogAt(t, e, "recent", 1<<20, now.Add(-time.Hour))

	// Compacted log has 10 MiB logical size but only 2 MiB kept (+ 1 MiB recent = 3 MiB <= 3.5 MiB).
	e.pruneLogsTo(7*24*time.Hour, 3<<20+1<<19)
	want := []string{"compacted.log", "compacted.log.dropped", "recent.log"}
	if got := logsLeft(t, e); !slices.Equal(got, want) {
		t.Fatalf("with compacted log within kept budget: logs left %q, want %q", got, want)
	}

	// Lower budget to 2.5 MiB so the older 2 MiB compacted log must now be pruned.
	e.pruneLogsTo(7*24*time.Hour, 2<<20+1<<19)
	want = []string{"recent.log"}
	if got := logsLeft(t, e); !slices.Equal(got, want) {
		t.Fatalf("past kept budget: logs left %q, want %q", got, want)
	}
}
