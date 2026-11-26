package worker

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The logs of commands the engine has forgotten are kept a week, and to
// 8 GiB on disk in all, the oldest going first past that. A command's own
// log is kept to its limit (EngineConfig.LogLimit) while it runs.
const (
	keepLogs   = 7 * 24 * time.Hour
	logsBudget = 8 << 30
)

func (e *Engine) logsDir() string { return filepath.Join(e.cfg.Dir, "logs") }

// noLog is the error for a log the node does not have.
func noLog(id string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no log of command %s on this node: logs of ended commands are kept %d days, and to %s in all",
		id, int(keepLogs.Hours()/24), gib(logsBudget))
}

// pruneLogs deletes the logs of commands the engine has forgotten that are
// past their retention, and prunes expired rotated daemon slog backups.
func (e *Engine) pruneLogs() {
	e.pruneLogsTo(keepLogs, logsBudget)
	if e.cfg.DaemonLogs != nil {
		e.cfg.DaemonLogs.Prune(e.now())
	}
}

// DaemonLogs queries this worker's rotating WARN/ERROR slog store.
func (e *Engine) DaemonLogs(req *pb.GetDaemonLogsRequest) (*pb.GetDaemonLogsResponse, error) {
	host := e.cfg.Host
	if host == "" {
		host = e.cfg.NodeID
	}
	return e.cfg.DaemonLogs.Query(host, "worker", host, req)
}

// pruneLogsTo deletes the logs of commands the engine no longer has that
// were last written more than keep ago, then the oldest of the rest until
// they take at most budget bytes on disk.
func (e *Engine) pruneLogsTo(keep time.Duration, budget int64) {
	ents, err := os.ReadDir(e.logsDir())
	if err != nil {
		slog.Error("list command logs", "err", err)
		return
	}
	e.mu.Lock()
	known := map[string]bool{}
	for id := range e.commands {
		known[id] = true
	}
	e.mu.Unlock()
	type logFile struct {
		path    string
		written time.Time
		disk    int64
	}
	var logs []logFile
	var total int64
	names := map[string]bool{}
	for _, ent := range ents {
		names[ent.Name()] = true
	}
	for _, ent := range ents {
		name := ent.Name()
		if e.leftover(name, names, ent) {
			if err := os.Remove(filepath.Join(e.logsDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("delete a log's leftover", "name", name, "err", err)
			}
			continue
		}
		id, ok := strings.CutSuffix(name, ".log")
		if !ok || known[id] {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue // gone meanwhile
		}
		path := filepath.Join(e.logsDir(), ent.Name())
		// A compacted log's punched holes take nothing; count kept bytes
		// rather than Stat_t.Blocks so budgeting is accurate on compressed
		// filesystems and before ZFS TXG commits (board 7650).
		l := logFile{path: path, written: info.ModTime(), disk: cmdlog.KeptBytes(path, info.Size())}
		logs = append(logs, l)
		total += l.disk
	}
	slices.SortFunc(logs, func(a, b logFile) int { return a.written.Compare(b.written) })
	cutoff := e.now().Add(-keep)
	for _, l := range logs {
		if !l.written.Before(cutoff) && total <= budget {
			break
		}
		if err := cmdlog.Remove(l.path); err != nil {
			slog.Error("delete command log", "path", l.path, "err", err)
			continue
		}
		total -= l.disk
	}
}

// leftover says whether the logs dir's entry name, of those in names, is
// left by what did not finish: a log's dropped record whose log is gone, or,
// an hour on, a record half written or a check of the filesystem.
func (e *Engine) leftover(name string, names map[string]bool, ent os.DirEntry) bool {
	if log, ok := strings.CutSuffix(name, ".log.dropped"); ok {
		return !names[log+".log"]
	}
	if log, ok := strings.CutSuffix(name, ".log.ts"); ok {
		return !names[log+".log"]
	}
	if !strings.Contains(name, ".log.dropped.") && !strings.HasPrefix(name, ".punch-check.") {
		return false
	}
	info, err := ent.Info()
	return err == nil && e.now().Sub(info.ModTime()) > time.Hour
}
