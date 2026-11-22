// Package profiler runs OpenTelemetry's always-on eBPF continuous profiler
// (go.opentelemetry.io/ebpf-profiler) on Linux workers, attributes every
// sampled stack trace to its hotbed command via cgroup v2, symbolizes native
// ELF/Go/Rust/C++ frames alongside kernel and interpreter frames, and makes
// collected profiles immediately queryable both live and after commands end.
package profiler

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	EngineName       = "opentelemetry-ebpf-profiler"
	DefaultSampleHz  = 20
	bucketDurationMs = 5000 // 5-second timeline buckets
	maxStacksPerCmd  = 4000
	maxSnapshotStack = 500
	maxSnapshotFuncs = 200
)

// Profiler runs continuous profiling on a worker and serves live and finished
// command profiles.
type Profiler interface {
	Start(ctx context.Context) error
	Stop()
	Status() *pb.ProfilerStatus
	Query(cmdID string, req *pb.GetCommandProfileRequest, live bool) *pb.CommandProfile
	Snapshot(cmdID string) *pb.CommandProfile
	Forget(cmdID string)
}

// ResolvedFrame is one symbolized frame in a captured stack trace.
type ResolvedFrame struct {
	Name       string
	Module     string
	FrameType  string // kernel, native, go, python, v8, ruby, php, hotspot, perl, dotnet, unknown
	SourceFile string
	SourceLine int32
}

// Sample is a single stack trace sample attributed to a hotbed command.
type Sample struct {
	CommandID string
	Timestamp time.Time
	Comm      string
	// Frames ordered root -> leaf (outermost caller first, innermost leaf last).
	Frames []ResolvedFrame
}

var cgroupCmdRe = regexp.MustCompile(`/hotbed-([a-z0-9]+)\.service(?:/|$)`)

// ExtractCommandIDFromCgroup extracts the hotbed command ID from /proc/<pid>/cgroup content.
func ExtractCommandIDFromCgroup(cgroupContent string) string {
	for _, line := range strings.Split(cgroupContent, "\n") {
		if m := cgroupCmdRe.FindStringSubmatch(line); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// ExtractCommandIDFromEnviron extracts HOTBED_COMMAND=<id> from NUL-delimited /proc/<pid>/environ.
func ExtractCommandIDFromEnviron(environ []byte) string {
	for _, entry := range strings.Split(string(environ), "\x00") {
		if v, ok := strings.CutPrefix(entry, "HOTBED_COMMAND="); ok && v != "" {
			return v
		}
	}
	return ""
}

type stackKey struct {
	stack    string
	leafType string
	comm     string
}

type bucketAcc struct {
	t          int64
	samples    int64
	kernel     int64
	user       int64
	leafCounts map[string]int64
}

type cmdAcc struct {
	startedAt int64
	updatedAt int64
	total     int64
	kernel    int64
	user      int64
	stacks    map[stackKey]int64
	// per-frame metadata (first seen) for enriching top_functions
	frameMeta map[string]ResolvedFrame
	comms     map[string]bool
	types     map[string]bool
	buckets   map[int64]*bucketAcc
}

// Store accumulates continuous profile samples per command in bounded memory.
type Store struct {
	sampleHz     int32
	totalSamples atomic.Int64

	mu   sync.RWMutex
	cmds map[string]*cmdAcc
}

// NewStore creates a new in-memory continuous profile store.
func NewStore(sampleHz int32) *Store {
	if sampleHz <= 0 {
		sampleHz = DefaultSampleHz
	}
	return &Store{
		sampleHz: sampleHz,
		cmds:     map[string]*cmdAcc{},
	}
}

// TotalSamples returns total samples observed across all commands.
func (s *Store) TotalSamples() int64 { return s.totalSamples.Load() }

// Record ingests one stack sample attributed to a hotbed command.
func (s *Store) Record(sample Sample) {
	s.totalSamples.Add(1)
	if sample.CommandID == "" || len(sample.Frames) == 0 {
		return
	}
	tsMs := sample.Timestamp.UnixMilli()
	if tsMs == 0 {
		tsMs = time.Now().UnixMilli()
	}
	names := make([]string, len(sample.Frames))
	for i, fr := range sample.Frames {
		names[i] = formatFrameLabel(fr)
	}
	leaf := sample.Frames[len(sample.Frames)-1]
	leafType := leaf.FrameType
	if leafType == "" {
		leafType = "native"
	}
	isKernel := leafType == "kernel"
	sk := stackKey{
		stack:    strings.Join(names, ";"),
		leafType: leafType,
		comm:     sample.Comm,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.cmds[sample.CommandID]
	if acc == nil {
		acc = &cmdAcc{
			startedAt: tsMs,
			updatedAt: tsMs,
			stacks:    map[stackKey]int64{},
			frameMeta: map[string]ResolvedFrame{},
			comms:     map[string]bool{},
			types:     map[string]bool{},
			buckets:   map[int64]*bucketAcc{},
		}
		s.cmds[sample.CommandID] = acc
	}
	if tsMs < acc.startedAt {
		acc.startedAt = tsMs
	}
	if tsMs > acc.updatedAt {
		acc.updatedAt = tsMs
	}
	acc.total++
	if isKernel {
		acc.kernel++
	} else {
		acc.user++
	}
	if sample.Comm != "" {
		acc.comms[sample.Comm] = true
	}
	for i, fr := range sample.Frames {
		if fr.FrameType != "" {
			acc.types[fr.FrameType] = true
		}
		lbl := names[i]
		if _, ok := acc.frameMeta[lbl]; !ok {
			acc.frameMeta[lbl] = fr
		}
	}
	if _, exists := acc.stacks[sk]; exists || len(acc.stacks) < maxStacksPerCmd {
		acc.stacks[sk]++
	}
	bt := (tsMs / bucketDurationMs) * bucketDurationMs
	b := acc.buckets[bt]
	if b == nil {
		b = &bucketAcc{t: bt, leafCounts: map[string]int64{}}
		acc.buckets[bt] = b
	}
	b.samples++
	if isKernel {
		b.kernel++
	} else {
		b.user++
	}
	b.leafCounts[names[len(names)-1]]++
}

// Snapshot returns a compact, self-contained CommandProfile suitable for
// persisting to disk or sending to the controller when a command finishes.
func (s *Store) Snapshot(cmdID string, status *pb.ProfilerStatus) *pb.CommandProfile {
	return s.Query(cmdID, &pb.GetCommandProfileRequest{Id: cmdID, Limit: maxSnapshotStack}, false, status)
}

// Forget drops in-memory profile data for a command after it has been persisted.
func (s *Store) Forget(cmdID string) {
	s.mu.Lock()
	delete(s.cmds, cmdID)
	s.mu.Unlock()
}

// Query builds a filtered CommandProfile view from live in-memory samples.
func (s *Store) Query(cmdID string, req *pb.GetCommandProfileRequest, live bool, status *pb.ProfilerStatus) *pb.CommandProfile {
	s.mu.RLock()
	acc := s.cmds[cmdID]
	if acc == nil {
		s.mu.RUnlock()
		return emptyProfile(cmdID, live, s.sampleHz, status)
	}
	// Copy out minimal state under read lock.
	startedAt, updatedAt := acc.startedAt, acc.updatedAt
	total, kernel, user := acc.total, acc.kernel, acc.user
	stacksCopy := make(map[stackKey]int64, len(acc.stacks))
	for k, v := range acc.stacks {
		stacksCopy[k] = v
	}
	metaCopy := make(map[string]ResolvedFrame, len(acc.frameMeta))
	for k, v := range acc.frameMeta {
		metaCopy[k] = v
	}
	comms := sortedKeys(acc.comms)
	types := sortedKeys(acc.types)
	buckets := make([]*pb.ProfileBucket, 0, len(acc.buckets))
	for _, b := range acc.buckets {
		topFn := ""
		var topCnt int64
		for fn, cnt := range b.leafCounts {
			if cnt > topCnt || (cnt == topCnt && fn < topFn) {
				topFn, topCnt = fn, cnt
			}
		}
		buckets = append(buckets, &pb.ProfileBucket{
			T:             b.t,
			Samples:       b.samples,
			KernelSamples: b.kernel,
			UserSamples:   b.user,
			TopFunction:   topFn,
		})
	}
	s.mu.RUnlock()

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].T < buckets[j].T })

	raw := &pb.CommandProfile{
		CommandId:     cmdID,
		Profiler:      EngineName,
		Live:          live,
		Active:        status.GetActive(),
		StatusNote:    status.GetError(),
		SampleHz:      s.sampleHz,
		StartedAt:     startedAt,
		UpdatedAt:     updatedAt,
		TotalSamples:  total,
		KernelSamples: kernel,
		UserSamples:   user,
		Comms:         comms,
		FrameTypes:    types,
		Buckets:       buckets,
	}
	for k, cnt := range stacksCopy {
		raw.Stacks = append(raw.Stacks, &pb.FoldedStack{
			Stack:    k.stack,
			Samples:  cnt,
			LeafType: k.leafType,
			Comm:     k.comm,
		})
	}
	return filterAndSummarize(raw, req, metaCopy)
}

// QueryProfile applies GetCommandProfileRequest filters to an already-persisted
// CommandProfile (e.g. loaded from controller SQLite for a finished command).
func QueryProfile(raw *pb.CommandProfile, req *pb.GetCommandProfileRequest) *pb.CommandProfile {
	if raw == nil {
		return nil
	}
	meta := make(map[string]ResolvedFrame, len(raw.TopFunctions))
	for _, fn := range raw.TopFunctions {
		meta[fn.Name] = ResolvedFrame{
			Name:       fn.Name,
			Module:     fn.Module,
			FrameType:  fn.FrameType,
			SourceFile: fn.SourceFile,
			SourceLine: fn.SourceLine,
		}
	}
	return filterAndSummarize(raw, req, meta)
}

func filterAndSummarize(raw *pb.CommandProfile, req *pb.GetCommandProfileRequest, meta map[string]ResolvedFrame) *pb.CommandProfile {
	q := strings.ToLower(strings.TrimSpace(req.GetQuery()))
	ftFilter := strings.ToLower(strings.TrimSpace(req.GetFrameType()))
	commFilter := strings.TrimSpace(req.GetComm())
	fromMs := req.GetFromMs()
	toMs := req.GetToMs()
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 200
	}

	// Filter timeline buckets by optional time window.
	var buckets []*pb.ProfileBucket
	var bucketSamples, bucketKernel, bucketUser int64
	for _, b := range raw.Buckets {
		if fromMs > 0 && b.T+bucketDurationMs < fromMs {
			continue
		}
		if toMs > 0 && b.T > toMs {
			continue
		}
		buckets = append(buckets, b)
		bucketSamples += b.Samples
		bucketKernel += b.KernelSamples
		bucketUser += b.UserSamples
	}

	var matchedStacks []*pb.FoldedStack
	var matchedSamples int64
	var matchedKernel, matchedUser int64
	type fnAcc struct {
		meta  ResolvedFrame
		self  int64
		total int64
	}
	fns := map[string]*fnAcc{}

	for _, st := range raw.Stacks {
		if commFilter != "" && st.Comm != commFilter {
			continue
		}
		if ftFilter != "" && strings.ToLower(st.LeafType) != ftFilter {
			// Also allow matching any frame in the stack of that type if metadata knows it.
			if !stackHasFrameType(st.Stack, ftFilter, meta) {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(st.Stack), q) && !strings.Contains(strings.ToLower(st.Comm), q) {
			continue
		}
		matchedStacks = append(matchedStacks, st)
		matchedSamples += st.Samples
		if st.LeafType == "kernel" {
			matchedKernel += st.Samples
		} else {
			matchedUser += st.Samples
		}

		frames := strings.Split(st.Stack, ";")
		seen := map[string]bool{}
		for i, lbl := range frames {
			if lbl == "" {
				continue
			}
			fa := fns[lbl]
			if fa == nil {
				m := meta[lbl]
				if m.Name == "" {
					m = inferFrameMeta(lbl, i == len(frames)-1, st.LeafType)
				}
				fa = &fnAcc{meta: m}
				fns[lbl] = fa
			}
			if !seen[lbl] {
				fa.total += st.Samples
				seen[lbl] = true
			}
			if i == len(frames)-1 {
				fa.self += st.Samples
			}
		}
	}

	sort.Slice(matchedStacks, func(i, j int) bool {
		if matchedStacks[i].Samples != matchedStacks[j].Samples {
			return matchedStacks[i].Samples > matchedStacks[j].Samples
		}
		return matchedStacks[i].Stack < matchedStacks[j].Stack
	})
	if len(matchedStacks) > limit {
		matchedStacks = matchedStacks[:limit]
	}

	topFns := make([]*pb.ProfileFunction, 0, len(fns))
	denom := float64(matchedSamples)
	for lbl, fa := range fns {
		name := fa.meta.Name
		if name == "" {
			name = lbl
		}
		var selfPct, totalPct float64
		if denom > 0 {
			selfPct = 100 * float64(fa.self) / denom
			totalPct = 100 * float64(fa.total) / denom
		}
		topFns = append(topFns, &pb.ProfileFunction{
			Name:         name,
			Module:       fa.meta.Module,
			FrameType:    fa.meta.FrameType,
			SourceFile:   fa.meta.SourceFile,
			SourceLine:   fa.meta.SourceLine,
			SelfSamples:  fa.self,
			TotalSamples: fa.total,
			SelfPct:      selfPct,
			TotalPct:     totalPct,
		})
	}
	sort.Slice(topFns, func(i, j int) bool {
		if topFns[i].SelfSamples != topFns[j].SelfSamples {
			return topFns[i].SelfSamples > topFns[j].SelfSamples
		}
		if topFns[i].TotalSamples != topFns[j].TotalSamples {
			return topFns[i].TotalSamples > topFns[j].TotalSamples
		}
		return topFns[i].Name < topFns[j].Name
	})
	fnLimit := min(limit, maxSnapshotFuncs)
	if len(topFns) > fnLimit {
		topFns = topFns[:fnLimit]
	}

	out := &pb.CommandProfile{
		CommandId:      raw.CommandId,
		Profiler:       raw.Profiler,
		Live:           raw.Live,
		Active:         raw.Active,
		StatusNote:     raw.StatusNote,
		SampleHz:       raw.SampleHz,
		StartedAt:      raw.StartedAt,
		UpdatedAt:      raw.UpdatedAt,
		TotalSamples:   raw.TotalSamples,
		MatchedSamples: matchedSamples,
		KernelSamples:  matchedKernel,
		UserSamples:    matchedUser,
		Comms:          slices.Clone(raw.Comms),
		FrameTypes:     slices.Clone(raw.FrameTypes),
		Stacks:         matchedStacks,
		TopFunctions:   topFns,
		Buckets:        buckets,
	}
	// If only a time-window filter was applied without stack filters, reflect bucket totals.
	if (fromMs > 0 || toMs > 0) && q == "" && ftFilter == "" && commFilter == "" && bucketSamples > 0 {
		out.MatchedSamples = bucketSamples
		out.KernelSamples = bucketKernel
		out.UserSamples = bucketUser
	}
	return out
}

func stackHasFrameType(stack, ft string, meta map[string]ResolvedFrame) bool {
	for _, lbl := range strings.Split(stack, ";") {
		if m, ok := meta[lbl]; ok && strings.ToLower(m.FrameType) == ft {
			return true
		}
	}
	return false
}

func formatFrameLabel(fr ResolvedFrame) string {
	name := strings.ReplaceAll(fr.Name, ";", ":")
	if name == "" {
		name = "[unknown]"
	}
	if fr.FrameType == "kernel" && !strings.HasSuffix(name, "_[k]") {
		return name + "_[k]"
	}
	return name
}

func inferFrameMeta(lbl string, isLeaf bool, leafType string) ResolvedFrame {
	ft := "native"
	if strings.HasSuffix(lbl, "_[k]") {
		ft = "kernel"
	} else if isLeaf && leafType != "" {
		ft = leafType
	}
	return ResolvedFrame{Name: lbl, FrameType: ft}
}

func emptyProfile(cmdID string, live bool, hz int32, status *pb.ProfilerStatus) *pb.CommandProfile {
	return &pb.CommandProfile{
		CommandId:  cmdID,
		Profiler:   EngineName,
		Live:       live,
		Active:     status.GetActive(),
		StatusNote: status.GetError(),
		SampleHz:   hz,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SaveProfile writes a gzip-compressed CommandProfile to path atomically.
func SaveProfile(path string, p *pb.CommandProfile) error {
	if p == nil {
		return nil
	}
	raw, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	gz := gzip.NewWriter(tmp)
	if _, err := gz.Write(raw); err != nil {
		gz.Close()
		tmp.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadProfile reads a gzip-compressed CommandProfile from path.
func LoadProfile(path string) (*pb.CommandProfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	b, err := io.ReadAll(io.LimitReader(gz, 16<<20))
	if err != nil {
		return nil, err
	}
	p := &pb.CommandProfile{}
	if err := proto.Unmarshal(b, p); err != nil {
		return nil, err
	}
	return p, nil
}

// IsNotExist reports whether err is a missing profile file.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
