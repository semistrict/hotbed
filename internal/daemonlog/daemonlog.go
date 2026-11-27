// Package daemonlog captures WARN and ERROR slog records from hotbed's
// controller and worker daemons into a size- and age-rotated JSONL log store
// inside each daemon's state directory, while forwarding all records unchanged
// to the daemon's stderr handler.
package daemonlog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	// DefaultMaxFileBytes is the size at which the active daemon log rotates.
	DefaultMaxFileBytes int64 = 4 << 20 // 4 MiB
	// DefaultMaxBackups is how many rotated backup files are kept alongside the
	// active file (5 files total, ~20 MiB cap per daemon).
	DefaultMaxBackups = 4
	// DefaultMaxAge is how long rotated backup files are kept before pruning.
	DefaultMaxAge = 14 * 24 * time.Hour
	// DefaultMaxRecordBytes caps any single serialized slog line so one huge
	// attribute cannot blow up disk or RPC payloads.
	DefaultMaxRecordBytes = 64 << 10 // 64 KiB
	// DefaultLimit is the default maximum entries returned by Query.
	DefaultLimit = 200
	// MaxLimit is the hard upper bound on entries returned by Query.
	MaxLimit = 1000

	activeFileName  = "daemon.jsonl"
	recentWindow    = 24 * time.Hour
	maxRecentItems  = 100_000
	maxStreamBuffer = 4096
)

// Config controls daemon log rotation and retention. Zero fields take defaults.
type Config struct {
	MaxFileBytes   int64
	MaxBackups     int
	MaxAge         time.Duration
	MaxRecordBytes int
	Now            func() time.Time
}

// DefaultConfig returns the standard rotation policy for controller and worker
// daemons: 4 MiB per file, 4 rotated backups (5 files / ~20 MiB max), 14 days
// retention.
func DefaultConfig() Config {
	return Config{
		MaxFileBytes:   DefaultMaxFileBytes,
		MaxBackups:     DefaultMaxBackups,
		MaxAge:         DefaultMaxAge,
		MaxRecordBytes: DefaultMaxRecordBytes,
		Now:            time.Now,
	}
}

func (c Config) withDefaults() Config {
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
	if c.MaxBackups < 0 {
		c.MaxBackups = 0
	}
	if c.MaxAge <= 0 {
		c.MaxAge = DefaultMaxAge
	}
	if c.MaxRecordBytes <= 0 {
		c.MaxRecordBytes = DefaultMaxRecordBytes
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type recentEntry struct {
	at    int64
	level pb.DaemonLogLevel
}

// Store persists WARN and ERROR slog records in rotating JSONL files under dir
// and buffers INFO, WARN, and ERROR records in a bounded in-memory ring for
// streaming to the worker's per-org WorkerDO.
// It is safe for concurrent use and never calls slog internally.
type Store struct {
	dir string
	cfg Config

	mu           sync.Mutex
	file         *os.File
	size         int64
	diskBytes    int64
	files        int32
	recent       []recentEntry
	lastAt       int64
	closed       bool
	streamEpoch  string
	streamSeq    int64
	streamBuf    []*pb.DaemonLogEntry
	streamKicked chan struct{}
}

func randomEpoch() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// Open creates dir if needed, prunes expired rotated files, opens the active
// daemon log for appending, and indexes recent 24h counts in memory.
func Open(dir string, cfg Config) (*Store, error) {
	cfg = cfg.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create daemon log dir %s: %w", dir, err)
	}
	s := &Store{
		dir:          dir,
		cfg:          cfg,
		streamEpoch:  randomEpoch(),
		streamKicked: make(chan struct{}, 1),
	}
	if err := s.openActiveLocked(); err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	s.pruneFilesLocked(now)
	s.refreshDiskLocked()
	s.loadRecentLocked(now)
	return s, nil
}

// Close closes the active log file. Subsequent writes are ignored.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// Handler returns an slog.Handler that forwards all records to primary (when
// non-nil), buffers INFO/WARN/ERROR records for streaming to WorkerDO, and
// persists WARN/ERROR records to s's rotating local files.
func (s *Store) Handler(primary slog.Handler) slog.Handler {
	fileHandler := slog.NewJSONHandler(&storeWriter{store: s}, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		AddSource:   true,
		ReplaceAttr: replaceAttr,
	})
	if primary == nil {
		return fileHandler
	}
	return &teeHandler{primary: primary, secondary: fileHandler}
}

// Epoch returns the per-process random epoch tagging streamed slog sequences.
func (s *Store) Epoch() string {
	if s == nil {
		return ""
	}
	return s.streamEpoch
}

// StreamKicked returns a channel signaled whenever new slog records are queued
// for streaming to WorkerDO.
func (s *Store) StreamKicked() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.streamKicked
}

// StreamStats reports how many entries are buffered for streaming and whether
// any buffered entry is WARN or ERROR (which should flush without debounce).
func (s *Store) StreamStats() (count int, urgent bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count = len(s.streamBuf)
	for _, e := range s.streamBuf {
		if e.Level == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN || e.Level == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR {
			urgent = true
			break
		}
	}
	return count, urgent
}

// PendingStream returns up to limit buffered slog entries fitting within
// maxBytes, oldest first, without removing them until AckStream is called.
func (s *Store) PendingStream(limit, maxBytes int) []*pb.DaemonLogEntry {
	if s == nil || limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streamBuf) == 0 {
		return nil
	}
	out := make([]*pb.DaemonLogEntry, 0, min(limit, len(s.streamBuf)))
	total := 0
	for _, e := range s.streamBuf {
		if len(out) >= limit {
			break
		}
		sz := proto.Size(e)
		if maxBytes > 0 && len(out) > 0 && total+sz > maxBytes {
			break
		}
		total += sz
		out = append(out, proto.Clone(e).(*pb.DaemonLogEntry))
	}
	return out
}

// AckStream drops all buffered stream entries with seq <= ackedSeq.
func (s *Store) AckStream(ackedSeq int64) {
	if s == nil || ackedSeq <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := 0
	for drop < len(s.streamBuf) && s.streamBuf[drop].Seq <= ackedSeq {
		drop++
	}
	if drop == 0 {
		return
	}
	remaining := len(s.streamBuf) - drop
	copy(s.streamBuf, s.streamBuf[drop:])
	for i := remaining; i < len(s.streamBuf); i++ {
		s.streamBuf[i] = nil
	}
	s.streamBuf = s.streamBuf[:remaining]
}

// Install wraps slog.Default() so all WARN and ERROR records are also captured
// in s, returning an idempotent restore function.
//
// slog.SetDefault points the log package's output at the new handler, and
// setting slog's own default handler back does not undo that: log would
// then write through slog's default handler, which writes through log, and
// deadlock on log's mutex. Restoring puts log's output and flags back too.
func Install(s *Store) func() {
	if s == nil {
		return func() {}
	}
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(s.Handler(prev.Handler())))
	var once sync.Once
	return func() {
		once.Do(func() {
			slog.SetDefault(prev)
			log.SetOutput(prevOut)
			log.SetFlags(prevFlags)
		})
	}
}

// Prune deletes rotated backup files older than MaxAge (and any stray backups
// beyond MaxBackups) and trims the in-memory 24h counter window.
func (s *Store) Prune(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pruneFilesLocked(now)
	s.refreshDiskLocked()
	s.trimRecentLocked(now)
}

// Summary returns O(1) in-memory rotation and 24h error/warn statistics without
// reading log files from disk.
func (s *Store) Summary(source, role, host string) *pb.DaemonLogSource {
	if s == nil {
		return &pb.DaemonLogSource{
			Source:    source,
			Role:      role,
			Host:      host,
			Reachable: true,
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaryLocked(source, role, host, s.cfg.Now())
}

func (s *Store) summaryLocked(source, role, host string, now time.Time) *pb.DaemonLogSource {
	s.trimRecentLocked(now)
	var errs, warns int32
	for _, r := range s.recent {
		switch r.level {
		case pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR:
			errs++
		case pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN:
			warns++
		}
	}
	out := &pb.DaemonLogSource{
		Source:       source,
		Role:         role,
		Host:         host,
		Reachable:    true,
		Errors_24H:   errs,
		Warns_24H:    warns,
		DiskBytes:    s.diskBytes,
		Files:        s.files,
		MaxFileBytes: s.cfg.MaxFileBytes,
		MaxFiles:     int32(s.cfg.MaxBackups + 1),
		MaxAgeDays:   int32(max(1, int(s.cfg.MaxAge/(24*time.Hour)))),
	}
	if s.lastAt > 0 {
		at := s.lastAt
		out.LastAt = &at
	}
	return out
}

// ParseSince parses a duration string such as "15m", "1h", "24h", or "7d".
func ParseSince(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	var err error
	if daysStr, ok := strings.CutSuffix(s, "d"); ok {
		days, perr := strconv.ParseFloat(daysStr, 64)
		if perr != nil {
			return 0, api.Errorf(api.CodeInvalidArgs, "invalid --since %q: use a duration such as 15m, 1h, 24h, or 7d", raw)
		}
		d = time.Duration(days * float64(24*time.Hour))
	} else {
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, api.Errorf(api.CodeInvalidArgs, "invalid --since %q: use a duration such as 15m, 1h, 24h, or 7d", raw)
		}
	}
	if d <= 0 {
		return 0, api.Errorf(api.CodeInvalidArgs, "--since %q must be positive", raw)
	}
	return d, nil
}

// NormalizeWindow validates and resolves time-window and limit parameters on req.
func NormalizeWindow(req *pb.GetDaemonLogsRequest, now time.Time) (fromMs, toMs int64, limit int, err error) {
	if req == nil {
		return 0, 0, DefaultLimit, nil
	}
	fromMs = req.FromMs
	toMs = req.ToMs
	if fromMs < 0 || toMs < 0 {
		return 0, 0, 0, api.Errorf(api.CodeInvalidArgs, "from-ms and to-ms must be non-negative")
	}
	if strings.TrimSpace(req.Since) != "" {
		if fromMs > 0 {
			return 0, 0, 0, api.Errorf(api.CodeInvalidArgs, "give --since or --from-ms, not both")
		}
		d, err := ParseSince(req.Since)
		if err != nil {
			return 0, 0, 0, err
		}
		fromMs = now.Add(-d).UnixMilli()
	}
	if fromMs > 0 && toMs > 0 && fromMs > toMs {
		return 0, 0, 0, api.Errorf(api.CodeInvalidArgs, "from-ms (%d) cannot be after to-ms (%d)", fromMs, toMs)
	}
	if req.Limit < 0 {
		return 0, 0, 0, api.Errorf(api.CodeInvalidArgs, "limit must be non-negative")
	}
	limit = int(req.Limit)
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	return fromMs, toMs, limit, nil
}

// Query reads matching WARN/ERROR entries newest-first across the active log
// and its rotated backups.
func (s *Store) Query(source, role, host string, req *pb.GetDaemonLogsRequest) (*pb.GetDaemonLogsResponse, error) {
	now := time.Now()
	if s != nil {
		now = s.cfg.Now()
	}
	fromMs, toMs, limit, err := NormalizeWindow(req, now)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return &pb.GetDaemonLogsResponse{
			Sources: []*pb.DaemonLogSource{{Source: source, Role: role, Host: host, Reachable: true}},
		}, nil
	}
	level := req.GetLevel()
	query := strings.ToLower(strings.TrimSpace(req.GetQuery()))

	// Snapshot open file handles under lock so queries never block writers
	// and never race with file rotation.
	s.mu.Lock()
	summary := s.summaryLocked(source, role, host, now)
	var files []*os.File
	for _, path := range s.existingPathsLocked() {
		f, oerr := os.Open(path)
		if oerr != nil {
			continue
		}
		files = append(files, f)
	}
	s.mu.Unlock()
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()

	entries := make([]*pb.DaemonLogEntry, 0, min(limit, 64))
	truncated := false
	for _, f := range files {
		lines, rerr := readLines(f)
		if rerr != nil {
			continue
		}
		for i := len(lines) - 1; i >= 0; i-- {
			e, ok := parseLine(lines[i], source, role, host)
			if !ok {
				continue
			}
			if level != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED && e.Level != level {
				continue
			}
			if level == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED && !req.GetIncludeInfo() && e.Level == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_INFO {
				continue
			}
			if fromMs > 0 && e.At < fromMs {
				continue
			}
			if toMs > 0 && e.At > toMs {
				continue
			}
			if query != "" && !matchesQuery(e, query) {
				continue
			}
			if len(entries) >= limit {
				truncated = true
				break
			}
			entries = append(entries, e)
		}
		if truncated {
			break
		}
	}

	return &pb.GetDaemonLogsResponse{
		Entries:   entries,
		Sources:   []*pb.DaemonLogSource{summary},
		Truncated: truncated,
	}, nil
}

func matchesQuery(e *pb.DaemonLogEntry, q string) bool {
	return strings.Contains(strings.ToLower(e.Msg), q) ||
		strings.Contains(strings.ToLower(e.AttrsJson), q) ||
		strings.Contains(strings.ToLower(e.SourceLoc), q) ||
		strings.Contains(strings.ToLower(e.Source), q) ||
		strings.Contains(strings.ToLower(e.Host), q)
}

func (s *Store) activePath() string { return filepath.Join(s.dir, activeFileName) }

func (s *Store) backupPath(n int) string {
	return filepath.Join(s.dir, fmt.Sprintf("daemon.%d.jsonl", n))
}

func (s *Store) existingPathsLocked() []string {
	paths := make([]string, 0, s.cfg.MaxBackups+1)
	if _, err := os.Stat(s.activePath()); err == nil {
		paths = append(paths, s.activePath())
	}
	for i := 1; i <= s.cfg.MaxBackups; i++ {
		p := s.backupPath(i)
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func (s *Store) openActiveLocked() error {
	f, err := os.OpenFile(s.activePath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", s.activePath(), err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	s.file = f
	s.size = info.Size()
	return nil
}

func (s *Store) appendLine(raw []byte) {
	line := normalizeRecord(raw, s.cfg.MaxRecordBytes)
	if len(line) == 0 {
		return
	}
	entry, parsed := parseLine(line, "", "", "")
	at, lvl := extractMeta(line)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	now := s.cfg.Now()
	if at == 0 {
		at = now.UnixMilli()
	}
	if parsed {
		if entry.At == 0 {
			entry.At = at
		}
		s.streamSeq++
		entry.Seq = s.streamSeq
		if len(s.streamBuf) >= maxStreamBuffer {
			copy(s.streamBuf, s.streamBuf[1:])
			s.streamBuf[len(s.streamBuf)-1] = entry
		} else {
			s.streamBuf = append(s.streamBuf, entry)
		}
		select {
		case s.streamKicked <- struct{}{}:
		default:
		}
	}
	// Local rotating JSONL files keep WARN and ERROR only; INFO is streamed to
	// WorkerDO without inflating local daemon-logs disk budgets.
	if lvl != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN && lvl != pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR {
		return
	}
	if s.file == nil {
		if err := s.openActiveLocked(); err != nil {
			return
		}
	}
	if s.size > 0 && s.size+int64(len(line)) > s.cfg.MaxFileBytes {
		if err := s.rotateLocked(now); err != nil {
			return
		}
	}
	n, err := s.file.Write(line)
	if err != nil {
		return
	}
	s.size += int64(n)
	s.diskBytes += int64(n)
	if s.files == 0 {
		s.files = 1
	}
	if at > s.lastAt {
		s.lastAt = at
	}
	if at >= now.Add(-recentWindow).UnixMilli() {
		s.recent = append(s.recent, recentEntry{at: at, level: lvl})
		if len(s.recent) > maxRecentItems {
			s.trimRecentLocked(now)
			if len(s.recent) > maxRecentItems {
				s.recent = s.recent[len(s.recent)-maxRecentItems:]
			}
		}
	}
}

func (s *Store) rotateLocked(now time.Time) error {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if s.cfg.MaxBackups == 0 {
		_ = os.Remove(s.activePath())
	} else {
		_ = os.Remove(s.backupPath(s.cfg.MaxBackups))
		for i := s.cfg.MaxBackups - 1; i >= 1; i-- {
			_ = os.Rename(s.backupPath(i), s.backupPath(i+1))
		}
		_ = os.Rename(s.activePath(), s.backupPath(1))
	}
	s.pruneFilesLocked(now)
	f, err := os.OpenFile(s.activePath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	s.size = 0
	s.refreshDiskLocked()
	s.trimRecentLocked(now)
	return nil
}

func (s *Store) pruneFilesLocked(now time.Time) {
	cutoff := now.Add(-s.cfg.MaxAge)
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		name := ent.Name()
		if name == activeFileName {
			continue
		}
		n, ok := parseBackupIndex(name)
		if !ok {
			continue
		}
		p := filepath.Join(s.dir, name)
		if n > s.cfg.MaxBackups {
			_ = os.Remove(p)
			continue
		}
		info, err := ent.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}

func (s *Store) refreshDiskLocked() {
	var total int64
	var count int32
	for _, p := range s.existingPathsLocked() {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
			count++
		}
	}
	s.diskBytes = total
	s.files = count
}

func (s *Store) loadRecentLocked(now time.Time) {
	cutoff := now.Add(-recentWindow).UnixMilli()
	var items []recentEntry
	var newest int64
	for _, p := range s.existingPathsLocked() {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		lines, rerr := readLines(f)
		_ = f.Close()
		if rerr != nil {
			continue
		}
		for _, line := range lines {
			at, lvl := extractMeta(line)
			if lvl == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED {
				continue
			}
			if at > newest {
				newest = at
			}
			if at >= cutoff {
				items = append(items, recentEntry{at: at, level: lvl})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at < items[j].at })
	if len(items) > maxRecentItems {
		items = items[len(items)-maxRecentItems:]
	}
	s.recent = items
	s.lastAt = newest
}

func (s *Store) trimRecentLocked(now time.Time) {
	cutoff := now.Add(-recentWindow).UnixMilli()
	idx := 0
	for idx < len(s.recent) && s.recent[idx].at < cutoff {
		idx++
	}
	if idx > 0 {
		s.recent = append(s.recent[:0], s.recent[idx:]...)
	}
}

func parseBackupIndex(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "daemon.")
	if !ok {
		return 0, false
	}
	numStr, ok := strings.CutSuffix(rest, ".jsonl")
	if !ok || numStr == "" {
		return 0, false
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func normalizeRecord(raw []byte, maxBytes int) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if len(trimmed)+1 <= maxBytes {
		out := make([]byte, len(trimmed)+1)
		copy(out, trimmed)
		out[len(trimmed)] = '\n'
		return out
	}
	// Preserve key fields in a compact truncated JSON line.
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		fallback, _ := json.Marshal(map[string]any{
			"time":      time.Now().UTC().Format(time.RFC3339Nano),
			"level":     "ERROR",
			"msg":       truncateUTF8(string(trimmed), 2048),
			"truncated": true,
		})
		return append(fallback, '\n')
	}
	slim := map[string]any{
		"truncated":      true,
		"original_bytes": len(trimmed),
	}
	for _, k := range []string{"time", "level", "source"} {
		if v, ok := obj[k]; ok {
			slim[k] = v
		}
	}
	if msg, ok := obj["msg"].(string); ok {
		slim["msg"] = truncateUTF8(msg, 2048)
	}
	if errVal, ok := obj["err"]; ok {
		slim["err"] = truncateUTF8(fmt.Sprint(errVal), 2048)
	}
	b, err := json.Marshal(slim)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	r := []rune(s)
	var b strings.Builder
	for _, ch := range r {
		if b.Len()+len(string(ch))+3 > maxBytes {
			break
		}
		b.WriteRune(ch)
	}
	b.WriteString("...")
	return b.String()
}

func readLines(r io.Reader) ([][]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	var lines [][]byte
	for sc.Scan() {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		lines = append(lines, cp)
	}
	return lines, sc.Err()
}

func extractMeta(line []byte) (int64, pb.DaemonLogLevel) {
	var raw struct {
		Time  string `json:"time"`
		Level string `json:"level"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return 0, pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED
	}
	return parseTimeMs(raw.Time), parseLevel(raw.Level)
}

func parseLine(line []byte, source, role, host string) (*pb.DaemonLogEntry, bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return nil, false
	}
	lvlStr, _ := obj["level"].(string)
	lvl := parseLevel(lvlStr)
	if lvl == pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED {
		return nil, false
	}
	timeStr, _ := obj["time"].(string)
	msg, _ := obj["msg"].(string)
	var sourceLoc string
	switch sv := obj["source"].(type) {
	case string:
		sourceLoc = sv
	case map[string]any:
		file, _ := sv["file"].(string)
		if file != "" {
			lineNum, _ := sv["line"].(float64)
			sourceLoc = shortSource(file, int(lineNum))
		}
	}
	delete(obj, "time")
	delete(obj, "level")
	delete(obj, "msg")
	delete(obj, "source")

	var attrsJSON string
	if len(obj) > 0 {
		if b, err := json.Marshal(obj); err == nil {
			attrsJSON = string(b)
		}
	}
	return &pb.DaemonLogEntry{
		At:        parseTimeMs(timeStr),
		Level:     lvl,
		Source:    source,
		Role:      role,
		Host:      host,
		Msg:       msg,
		AttrsJson: attrsJSON,
		SourceLoc: sourceLoc,
	}, true
}

func parseTimeMs(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func parseLevel(s string) pb.DaemonLogLevel {
	up := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(up, "ERROR"):
		return pb.DaemonLogLevel_DAEMON_LOG_LEVEL_ERROR
	case strings.HasPrefix(up, "WARN"):
		return pb.DaemonLogLevel_DAEMON_LOG_LEVEL_WARN
	case strings.HasPrefix(up, "INFO"):
		return pb.DaemonLogLevel_DAEMON_LOG_LEVEL_INFO
	default:
		return pb.DaemonLogLevel_DAEMON_LOG_LEVEL_UNSPECIFIED
	}
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.SourceKey {
		if src, ok := a.Value.Any().(*slog.Source); ok && src != nil && src.File != "" {
			return slog.String(slog.SourceKey, shortSource(src.File, src.Line))
		}
	}
	return a
}

func shortSource(file string, line int) string {
	slash := filepath.ToSlash(file)
	for _, marker := range []string{"/internal/", "/cmd/", "/skills/"} {
		if i := strings.Index(slash, marker); i >= 0 {
			slash = slash[i+1:]
			break
		}
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", slash, line)
	}
	return slash
}

type storeWriter struct {
	store *Store
}

func (w *storeWriter) Write(p []byte) (int, error) {
	w.store.appendLine(p)
	return len(p), nil
}

type teeHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level) || h.secondary.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var err1, err2 error
	if h.primary.Enabled(ctx, r.Level) {
		err1 = h.primary.Handle(ctx, r.Clone())
	}
	if h.secondary.Enabled(ctx, r.Level) {
		err2 = h.secondary.Handle(ctx, r.Clone())
	}
	return errors.Join(err1, err2)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{
		primary:   h.primary.WithAttrs(attrs),
		secondary: h.secondary.WithAttrs(attrs),
	}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{
		primary:   h.primary.WithGroup(name),
		secondary: h.secondary.WithGroup(name),
	}
}
