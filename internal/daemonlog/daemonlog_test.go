package daemonlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestStoreCapturesOnlyWarnAndErrorAndForwardsAll(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	st, err := Open(dir, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var stderr bytes.Buffer
	logger := slog.New(st.Handler(slog.NewJSONHandler(&stderr, nil)))
	logger.Info("startup complete", "port", 7120)
	logger.With("component", "reaper").Warn("release workspace later", "workspace", "ws-1")
	logger.WithGroup("db").Error("prune failed", "err", "busy")

	// Primary stderr receives all three (INFO, WARN, ERROR).
	if got := strings.Count(strings.TrimSpace(stderr.String()), "\n") + 1; got != 3 {
		t.Fatalf("stderr got %d lines, want 3:\n%s", got, stderr.String())
	}

	res, err := st.Query("controller", "controller", "ubu1", &pb.GetDaemonLogsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(res.Entries), res.Entries)
	}
	// Newest first.
	if res.Entries[0].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR || res.Entries[0].Msg != "prune failed" {
		t.Fatalf("entry[0] = %+v", res.Entries[0])
	}
	if !strings.Contains(res.Entries[0].AttrsJson, `"db"`) || !strings.Contains(res.Entries[0].AttrsJson, `"busy"`) {
		t.Errorf("entry[0] attrs = %q, want db.err=busy", res.Entries[0].AttrsJson)
	}
	if !strings.Contains(res.Entries[0].SourceLoc, "daemonlog_test.go:") {
		t.Errorf("entry[0] sourceLoc = %q, want daemonlog_test.go:<line>", res.Entries[0].SourceLoc)
	}
	if res.Entries[1].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN || res.Entries[1].Msg != "release workspace later" {
		t.Fatalf("entry[1] = %+v", res.Entries[1])
	}
	if !strings.Contains(res.Entries[1].AttrsJson, `"component":"reaper"`) {
		t.Errorf("entry[1] attrs = %q, want component=reaper", res.Entries[1].AttrsJson)
	}

	sum := st.Summary("controller", "controller", "ubu1")
	if sum.Errors_24H != 1 || sum.Warns_24H != 1 || sum.Files != 1 || sum.DiskBytes <= 0 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestRotationBoundsFilesAndPreservesNewestOrder(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	st, err := Open(dir, Config{
		MaxFileBytes: 450,
		MaxBackups:   2,
		MaxAge:       7 * 24 * time.Hour,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	logger := slog.New(st.Handler(nil))
	for i := range 25 {
		logger.Error(fmt.Sprintf("msg-%02d", i), "detail", strings.Repeat("x", 60))
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > 3 {
		t.Fatalf("expected at most 3 files (active + 2 backups), got %d", len(ents))
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.1.jsonl")); err != nil {
		t.Fatalf("expected rotated daemon.1.jsonl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.3.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("daemon.3.jsonl should have been dropped beyond MaxBackups=2")
	}

	res, err := st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) == 0 || len(res.Entries) >= 25 {
		t.Fatalf("expected rotated subset of entries, got %d", len(res.Entries))
	}
	if res.Entries[0].Msg != "msg-24" {
		t.Errorf("newest entry = %q, want msg-24", res.Entries[0].Msg)
	}
	// Verify strictly descending sequence across file boundaries.
	prev := 25
	for _, e := range res.Entries {
		var n int
		if _, err := fmt.Sscanf(e.Msg, "msg-%02d", &n); err != nil {
			t.Fatalf("parse %q: %v", e.Msg, err)
		}
		if n >= prev {
			t.Fatalf("entries out of newest-first order: got %d after %d", n, prev)
		}
		prev = n
	}

	sum := st.Summary("ubu1", "worker", "ubu1")
	if sum.Files != int32(len(ents)) || sum.MaxFiles != 3 || sum.MaxFileBytes != 450 {
		t.Errorf("unexpected summary: %+v", sum)
	}
}

func TestPruneRemovesExpiredBackupsAndPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	st, err := Open(dir, Config{
		MaxFileBytes: 250,
		MaxBackups:   3,
		MaxAge:       48 * time.Hour,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(st.Handler(nil))
	for i := range 6 {
		logger.Warn(fmt.Sprintf("w-%d", i), "pad", strings.Repeat("a", 50))
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Age daemon.1.jsonl past MaxAge.
	oldTime := now.Add(-72 * time.Hour)
	b1 := filepath.Join(dir, "daemon.1.jsonl")
	if err := os.Chtimes(b1, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Reopening prunes expired backups and recovers 24h counts from remaining files.
	st2, err := Open(dir, Config{
		MaxFileBytes: 250,
		MaxBackups:   3,
		MaxAge:       48 * time.Hour,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	if _, err := os.Stat(b1); !os.IsNotExist(err) {
		t.Fatalf("expired backup daemon.1.jsonl was not pruned on reopen")
	}
	res, err := st2.Query("controller", "controller", "ubu1", &pb.GetDaemonLogsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) == 0 {
		t.Fatalf("expected remaining active entries after reopen")
	}
	sum := st2.Summary("controller", "controller", "ubu1")
	if sum.Warns_24H == 0 {
		t.Errorf("expected reopened store to recover 24h warn count, got %+v", sum)
	}
}

func TestQueryFiltersAndOversizedRecordTruncation(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	st, err := Open(dir, Config{
		MaxRecordBytes: 512,
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	logger := slog.New(st.Handler(nil))
	logger.Warn("cache miss", "repo", "cladefs")
	logger.Error("disk full", "err", strings.Repeat("Z", 2000))

	// Level filter.
	errs, err := st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{
		Level: pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs.Entries) != 1 || errs.Entries[0].Msg != "disk full" {
		t.Fatalf("error filter = %+v", errs.Entries)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(errs.Entries[0].AttrsJson), &attrs); err != nil {
		t.Fatalf("truncated record must stay valid JSON: %v (%s)", err, errs.Entries[0].AttrsJson)
	}
	if attrs["truncated"] != true {
		t.Errorf("expected truncated=true in attrs: %v", attrs)
	}

	// Substring query filter.
	q, err := st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{Query: "CLADEFS"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Entries) != 1 || q.Entries[0].Msg != "cache miss" {
		t.Fatalf("query filter = %+v", q.Entries)
	}

	// Limit + truncated flag.
	lim, err := st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(lim.Entries) != 1 || !lim.Truncated {
		t.Fatalf("limit=1 got len=%d truncated=%v", len(lim.Entries), lim.Truncated)
	}
}

func TestConcurrentWritesAndQueries(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{MaxFileBytes: 600, MaxBackups: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	logger := slog.New(st.Handler(nil))
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 30 {
				logger.Warn("concurrent warn", "g", id, "i", i)
				if i%5 == 0 {
					_, _ = st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{Limit: 10})
					_ = st.Summary("ubu1", "worker", "ubu1")
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestParseSince(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"2h", 2 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
	} {
		got, err := ParseSince(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseSince(%q) = (%v, %v), want (%v, nil)", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"-5m", "0s", "bogus", "-2d"} {
		if _, err := ParseSince(bad); err == nil {
			t.Errorf("ParseSince(%q) succeeded, want error", bad)
		}
	}
}

// Restoring what Install replaced puts the log package back as it was: left
// pointing at the replaced handler, log would write through slog's default
// handler, which writes through log, and deadlock on log's mutex.
func TestInstallRestoresTheLogPackage(t *testing.T) {
	st, err := Open(t.TempDir(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var out bytes.Buffer
	log.SetOutput(&out)
	log.SetFlags(log.Lshortfile)

	Install(st)()

	if log.Writer() != &out || log.Flags() != log.Lshortfile {
		t.Fatalf("after restore log writes to %T with flags %d, want the buffer with flags %d", log.Writer(), log.Flags(), log.Lshortfile)
	}
	slog.Info("after restore")
	if !strings.Contains(out.String(), "after restore") {
		t.Errorf("slog.Info after restore wrote %q, want it in log's output", out.String())
	}
}

func TestStoreStreamBufferCapturesInfoWarnErrorAndAcks(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	st, err := Open(t.TempDir(), Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Epoch() == "" {
		t.Fatal("expected non-empty stream epoch")
	}

	logger := slog.New(st.Handler(nil))
	logger.Info("auto-captured base", "baseId", "hotbed-vm-cap-1")
	if count, urgent := st.StreamStats(); count != 1 || urgent {
		t.Fatalf("after INFO: StreamStats = (%d, %v), want (1, false)", count, urgent)
	}
	// Local JSONL disk file stays WARN/ERROR only.
	diskRes, err := st.Query("ubu1", "worker", "ubu1", &pb.GetDaemonLogsRequest{IncludeInfo: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(diskRes.Entries) != 0 {
		t.Fatalf("expected 0 local disk entries for INFO, got %+v", diskRes.Entries)
	}

	logger.Warn("retry cell busy", "cell", 42)
	logger.Error("disk offline", "err", "io")
	if count, urgent := st.StreamStats(); count != 3 || !urgent {
		t.Fatalf("after WARN+ERROR: StreamStats = (%d, %v), want (3, true)", count, urgent)
	}

	batch := st.PendingStream(2, 0)
	if len(batch) != 2 || batch[0].Seq != 1 || batch[0].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_INFO || batch[1].Seq != 2 || batch[1].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN {
		t.Fatalf("PendingStream(2) = %+v", batch)
	}
	st.AckStream(2)
	rest := st.PendingStream(10, 0)
	if len(rest) != 1 || rest[0].Seq != 3 || rest[0].Level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR {
		t.Fatalf("remaining after AckStream(2) = %+v", rest)
	}
	st.AckStream(3)
	if count, urgent := st.StreamStats(); count != 0 || urgent {
		t.Fatalf("after full Ack: StreamStats = (%d, %v), want (0, false)", count, urgent)
	}
}
