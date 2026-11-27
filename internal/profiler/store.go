// Package profiler runs OpenTelemetry's always-on eBPF continuous profiler
// (go.opentelemetry.io/ebpf-profiler) on Linux workers, attributes every
// sampled stack trace to its hotbed command via cgroup v2, symbolizes native
// ELF/Go/Rust/C++ frames alongside kernel and interpreter frames, and makes
// collected profiles immediately queryable both live and after commands end.
package profiler

import (
	"cmp"
	"context"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	EngineName       = "opentelemetry-ebpf-profiler"
	DefaultSampleHz  = 20
	bucketDurationMs = 5000 // 5-second timeline buckets
	// noTopFrame marks a ProfileBucket with no recorded leaf frame (e.g.
	// when the command frame table is saturated and only unseen leaves arrived).
	noTopFrame uint32 = math.MaxUint32
	// maxCmdBuckets bounds a command's timeline: past it, its buckets
	// double in length, two merging into one.
	maxCmdBuckets    = 720
	maxStacksPerCmd  = 4000
	maxSnapshotStack = 500
	maxSnapshotFuncs = 200
	// The node's ring buffer: 30 minutes of active 5-second buckets, each
	// with at most so many distinct stacks, and the frames table they share
	// compacted to the frames they still name once it outgrows these.
	defaultMaxNodeBuckets = 360
	maxStacksPerBucket    = 1000
	defaultMaxNodeFrames  = 32768
	// Per-command bounds on interned frames and process comm names so
	// long-running commands with JIT/dynamic frames cannot grow RAM without bound.
	defaultMaxCmdFrames = 65536
	defaultMaxCmdComms  = 256
	// MaxProfileBytes bounds an encoded CommandProfile, a query's reply or
	// the snapshot a finished command's event carries to the controller,
	// well under the 4 MiB Connect lets a message have: the least sampled
	// stacks give way until it fits.
	MaxProfileBytes = 1 << 20
)

// Profiler runs continuous profiling on a worker and serves live and finished
// command profiles as well as host-wide node profiles.
type Profiler interface {
	Start(ctx context.Context) error
	Stop()
	Status() *pb.ProfilerStatus
	Query(cmdID string, req *pb.GetProfileRequest, live bool) *pb.CommandProfile
	QueryNode(node string, req *pb.GetProfileRequest) *pb.CommandProfile
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

var (
	cgroupCmdRe      = regexp.MustCompile(`/hotbed-([a-z0-9]+)\.service(?:/|$)`)
	guestCgroupCmdRe = regexp.MustCompile(`/hotbed/([a-zA-Z0-9][a-zA-Z0-9._-]*)(?:/|$)`)
)

// ExtractCommandIDFromCgroup extracts the hotbed command ID from /proc/<pid>/cgroup content.
func ExtractCommandIDFromCgroup(cgroupContent string) string {
	for _, line := range strings.Split(cgroupContent, "\n") {
		if m := cgroupCmdRe.FindStringSubmatch(line); len(m) == 2 {
			return m[1]
		}
		if m := guestCgroupCmdRe.FindStringSubmatch(line); len(m) == 2 {
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

type bucketAcc struct {
	t          int64
	samples    int64
	kernel     int64
	user       int64
	stacks     map[uint32]int64 // stackID in cmdAcc.stackList -> samples
	leafCounts map[uint32]int64
}

// topFrame returns the leaf sampled most in leafCounts, the first frame among
// equals, or noTopFrame if empty.
func topFrame(leafCounts map[uint32]int64) uint32 {
	if len(leafCounts) == 0 {
		return noTopFrame
	}
	var top uint32
	var topCnt int64
	for id, cnt := range leafCounts {
		if cnt > topCnt || (cnt == topCnt && id < top) {
			top, topCnt = id, cnt
		}
	}
	return top
}

func (b *bucketAcc) top() uint32 { return topFrame(b.leafCounts) }

type cmdAcc struct {
	startedAt int64
	updatedAt int64
	total     int64
	kernel    int64
	user      int64
	// frames lists each function seen once; stacks and leafCounts name frames
	// by index here.
	frames flame.Table
	// stackIndex and stackList intern distinct stacks per command (capped at
	// Store.maxCmdStacks) so buckets only hold compact uint32 stack IDs.
	stackIndex map[flame.StackKey]uint32
	stackList  []flame.StackKey
	comms      map[string]bool
	types      map[string]bool
	buckets    map[int64]*bucketAcc
	// bucketMs is how long each of buckets is.
	bucketMs int64
}

// frameID is fr's index in t, added if t lacks its function.
func frameID(t *flame.Table, fr ResolvedFrame) uint32 {
	if id, ok := t.Lookup(fr.Name, fr.FrameType); ok {
		return id
	}
	return t.ID(&pb.ProfileFrame{
		Name: fr.Name, Module: fr.Module, FrameType: fr.FrameType, SourceFile: fr.SourceFile, SourceLine: fr.SourceLine,
	})
}

type nodeBucket struct {
	t          int64
	startedAt  int64
	updatedAt  int64
	samples    int64
	kernel     int64
	user       int64
	stacks     flame.Stacks // naming frames in Store.nodeFrames
	leafCounts map[uint32]int64
	comms      map[string]bool
	types      map[string]bool
}

func (b *nodeBucket) top() uint32 { return topFrame(b.leafCounts) }

// Store accumulates continuous profile samples per command and in a bounded
// host-wide ring buffer of 5-second buckets for the node.
type Store struct {
	sampleHz       int32
	maxNodeBuckets int
	maxNodeFrames  int
	maxCmdFrames   int
	maxCmdComms    int
	maxCmdStacks   int
	totalSamples   atomic.Int64

	mu          sync.RWMutex
	cmds        map[string]*cmdAcc
	nodeBuckets map[int64]*nodeBucket
	// nodeFrames lists the functions the node's buckets name; nodeKept is
	// how many it had after it was last compacted.
	nodeFrames flame.Table
	nodeKept   int
}

// NewStore creates a new in-memory continuous profile store.
func NewStore(sampleHz int32) *Store {
	if sampleHz <= 0 {
		sampleHz = DefaultSampleHz
	}
	return &Store{
		sampleHz:       sampleHz,
		maxNodeBuckets: defaultMaxNodeBuckets,
		maxNodeFrames:  defaultMaxNodeFrames,
		maxCmdFrames:   defaultMaxCmdFrames,
		maxCmdComms:    defaultMaxCmdComms,
		maxCmdStacks:   maxStacksPerCmd,
		cmds:           map[string]*cmdAcc{},
		nodeBuckets:    map[int64]*nodeBucket{},
	}
}

// TotalSamples returns total samples observed across the node.
func (s *Store) TotalSamples() int64 { return s.totalSamples.Load() }

// Record ingests one stack sample into the node ring buffer and, when
// attributed to a hotbed command, into that command's accumulator.
func (s *Store) Record(sample Sample) {
	s.totalSamples.Add(1)
	if len(sample.Frames) == 0 {
		return
	}
	tsMs := sample.Timestamp.UnixMilli()
	if tsMs == 0 {
		tsMs = time.Now().UnixMilli()
	}
	leafType := sample.Frames[len(sample.Frames)-1].FrameType
	if leafType == "" {
		leafType = "native"
	}
	isKernel := leafType == "kernel"
	bt := (tsMs / bucketDurationMs) * bucketDurationMs

	s.mu.Lock()
	defer s.mu.Unlock()

	s.recordNodeLocked(tsMs, bt, sample, leafType, isKernel)

	if sample.CommandID == "" {
		return
	}
	acc := s.cmds[sample.CommandID]
	if acc == nil {
		acc = &cmdAcc{
			startedAt:  tsMs,
			updatedAt:  tsMs,
			stackIndex: map[flame.StackKey]uint32{},
			comms:      map[string]bool{},
			types:      map[string]bool{},
			buckets:    map[int64]*bucketAcc{},
			bucketMs:   bucketDurationMs,
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
		if _, ok := acc.comms[sample.Comm]; ok || len(acc.comms) < s.maxCmdComms {
			acc.comms[sample.Comm] = true
		}
	}
	cbt := tsMs / acc.bucketMs * acc.bucketMs
	b := acc.buckets[cbt]
	if b == nil {
		b = &bucketAcc{
			t:          cbt,
			stacks:     map[uint32]int64{},
			leafCounts: map[uint32]int64{},
		}
		acc.buckets[cbt] = b
	}
	b.samples++
	if isKernel {
		b.kernel++
	} else {
		b.user++
	}

	// Look up all frames before interning so rejected stacks never grow
	// acc.frames or acc.stackList.
	ids := make([]byte, 0, 4*len(sample.Frames))
	allKnown := true
	var leaf uint32
	for _, fr := range sample.Frames {
		id, ok := acc.frames.Lookup(fr.Name, fr.FrameType)
		if !ok {
			allKnown = false
			break
		}
		leaf = id
		ids = flame.AppendID(ids, id)
	}
	if allKnown {
		sk := flame.StackKey{Frames: string(ids), LeafType: leafType, Comm: sample.Comm}
		if sid, known := acc.stackIndex[sk]; known {
			if _, exists := b.stacks[sid]; exists || len(b.stacks) < maxStacksPerBucket {
				b.stacks[sid]++
			}
		} else if len(acc.stackList) < s.maxCmdStacks && len(b.stacks) < maxStacksPerBucket {
			sid = uint32(len(acc.stackList))
			acc.stackList = append(acc.stackList, sk)
			acc.stackIndex[sk] = sid
			b.stacks[sid]++
		}
		b.leafCounts[leaf]++
	} else if len(acc.stackList) < s.maxCmdStacks && len(b.stacks) < maxStacksPerBucket && canInternFrames(&acc.frames, sample.Frames, s.maxCmdFrames) {
		ids = ids[:0]
		for _, fr := range sample.Frames {
			if fr.FrameType != "" {
				acc.types[fr.FrameType] = true
			}
			leaf = frameID(&acc.frames, fr)
			ids = flame.AppendID(ids, leaf)
		}
		sk := flame.StackKey{Frames: string(ids), LeafType: leafType, Comm: sample.Comm}
		sid := uint32(len(acc.stackList))
		acc.stackList = append(acc.stackList, sk)
		acc.stackIndex[sk] = sid
		b.stacks[sid]++
		b.leafCounts[leaf]++
	} else {
		leafFr := sample.Frames[len(sample.Frames)-1]
		if id, ok := acc.frames.Lookup(leafFr.Name, leafFr.FrameType); ok {
			b.leafCounts[id]++
		}
	}
	for len(acc.buckets) > maxCmdBuckets {
		acc.coarsen()
	}
}

// canInternFrames reports whether interning all frames of stack into t stays
// within maxFrames without mutating t.
func canInternFrames(t *flame.Table, frames []ResolvedFrame, maxFrames int) bool {
	cur := len(t.Frames())
	if cur >= maxFrames {
		return false
	}
	if cur+len(frames) <= maxFrames {
		return true
	}
	seen := make(map[[2]string]struct{}, len(frames))
	missing := 0
	for _, fr := range frames {
		if _, ok := t.Lookup(fr.Name, fr.FrameType); ok {
			continue
		}
		k := [2]string{fr.Name, fr.FrameType}
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			missing++
			if cur+missing > maxFrames {
				return false
			}
		}
	}
	return true
}

// coarsen doubles the length of a's buckets, merging the two that each new
// one spans, so a command that runs for days keeps a timeline of at most
// maxCmdBuckets.
func (a *cmdAcc) coarsen() {
	a.bucketMs *= 2
	merged := make(map[int64]*bucketAcc, len(a.buckets)/2+1)
	for _, b := range a.buckets {
		t := b.t / a.bucketMs * a.bucketMs
		m := merged[t]
		if m == nil {
			m = &bucketAcc{
				t:          t,
				stacks:     make(map[uint32]int64, len(b.stacks)),
				leafCounts: make(map[uint32]int64, len(b.leafCounts)),
			}
			merged[t] = m
		}
		m.samples += b.samples
		m.kernel += b.kernel
		m.user += b.user
		for sid, n := range b.stacks {
			m.stacks[sid] += n
		}
		for id, n := range b.leafCounts {
			m.leafCounts[id] += n
		}
	}
	a.buckets = merged
}

func (s *Store) recordNodeLocked(tsMs, bt int64, sample Sample, leafType string, isKernel bool) {
	b := s.nodeBuckets[bt]
	if b == nil {
		if len(s.nodeBuckets) >= s.maxNodeBuckets {
			var oldest int64
			first := true
			for k := range s.nodeBuckets {
				if first || k < oldest {
					oldest = k
					first = false
				}
			}
			if !first {
				if bt < oldest {
					return
				}
				delete(s.nodeBuckets, oldest)
			}
		}
		b = &nodeBucket{
			t:          bt,
			startedAt:  tsMs,
			updatedAt:  tsMs,
			stacks:     flame.Stacks{},
			leafCounts: map[uint32]int64{},
			comms:      map[string]bool{},
			types:      map[string]bool{},
		}
		s.nodeBuckets[bt] = b
	}
	if tsMs < b.startedAt {
		b.startedAt = tsMs
	}
	if tsMs > b.updatedAt {
		b.updatedAt = tsMs
	}
	b.samples++
	if isKernel {
		b.kernel++
	} else {
		b.user++
	}
	if sample.Comm != "" {
		b.comms[sample.Comm] = true
	}
	// Host-wide, a stack is rooted at its process.
	key := make([]byte, 0, 4*(len(sample.Frames)+1))
	if comm := strings.TrimSpace(sample.Comm); comm != "" && sample.Frames[0].Name != comm {
		key = flame.AppendID(key, frameID(&s.nodeFrames, ResolvedFrame{Name: comm, FrameType: "process"}))
	}
	var leaf uint32
	for _, fr := range sample.Frames {
		if fr.FrameType != "" {
			b.types[fr.FrameType] = true
		}
		leaf = frameID(&s.nodeFrames, fr)
		key = flame.AppendID(key, leaf)
	}
	nsk := flame.StackKey{Frames: string(key), LeafType: leafType, Comm: sample.Comm}
	if _, exists := b.stacks[nsk]; exists || len(b.stacks) < maxStacksPerBucket {
		b.stacks[nsk]++
	}
	b.leafCounts[leaf]++
	if n := len(s.nodeFrames.Frames()); n > s.maxNodeFrames && n > 2*s.nodeKept {
		s.compactNodeFramesLocked()
	}
}

// compactNodeFramesLocked rebuilds the node's frames table from the frames
// its buckets still name, dropping those of buckets the ring has let go.
func (s *Store) compactNodeFramesLocked() {
	old := s.nodeFrames.Frames()
	var t flame.Table
	for _, b := range s.nodeBuckets {
		stacks := make(flame.Stacks, len(b.stacks))
		for k, cnt := range b.stacks {
			key := make([]byte, 0, len(k.Frames))
			for _, id := range flame.IDs(k.Frames) {
				key = flame.AppendID(key, t.ID(old[id]))
			}
			k.Frames = string(key)
			stacks[k] += cnt
		}
		b.stacks = stacks
		leaves := make(map[uint32]int64, len(b.leafCounts))
		for id, cnt := range b.leafCounts {
			leaves[t.ID(old[id])] += cnt
		}
		b.leafCounts = leaves
	}
	s.nodeFrames, s.nodeKept = t, len(t.Frames())
}

// Snapshot returns a compact, self-contained CommandProfile suitable for
// persisting to disk or sending to the controller when a command finishes.
func (s *Store) Snapshot(cmdID string, status *pb.ProfilerStatus) *pb.CommandProfile {
	return s.Query(cmdID, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: cmdID},
		Limit:  maxSnapshotStack,
	}, false, status)
}

// Forget drops in-memory profile data for a command after it has been persisted.
func (s *Store) Forget(cmdID string) {
	s.mu.Lock()
	delete(s.cmds, cmdID)
	s.mu.Unlock()
}

// Query builds a filtered CommandProfile view from live in-memory samples.
// Time-range filtering (from_ms / to_ms) filters buckets, stacks, and top functions.
func (s *Store) Query(cmdID string, req *pb.GetProfileRequest, live bool, status *pb.ProfilerStatus) *pb.CommandProfile {
	fromMs := req.GetFromMs()
	toMs := req.GetToMs()

	s.mu.RLock()
	acc := s.cmds[cmdID]
	if acc == nil {
		s.mu.RUnlock()
		return emptyProfile(cmdID, live, s.sampleHz, status)
	}
	// Frames are never changed once recorded, so the copy shares them.
	raw := &pb.CommandProfile{
		CommandId:    cmdID,
		Profiler:     EngineName,
		Live:         live,
		Active:       status.GetActive(),
		StatusNote:   status.GetError(),
		SampleHz:     s.sampleHz,
		StartedAt:    acc.startedAt,
		UpdatedAt:    acc.updatedAt,
		TotalSamples: acc.total,
		Comms:        sortedKeys(acc.comms),
		FrameTypes:   sortedKeys(acc.types),
		Frames:       slices.Clone(acc.frames.Frames()),
	}
	type bucketStackSnap struct {
		sid     uint32
		samples int64
	}
	type bucketSnap struct {
		pb     *pb.ProfileBucket
		stacks []bucketStackSnap
	}
	stackTotals := map[uint32]int64{}
	bucketSnaps := make([]bucketSnap, 0, len(acc.buckets))
	for _, b := range acc.buckets {
		if fromMs > 0 && b.t+acc.bucketMs <= fromMs {
			continue
		}
		if toMs > 0 && b.t > toMs {
			continue
		}
		raw.KernelSamples += b.kernel
		raw.UserSamples += b.user
		bs := make([]bucketStackSnap, 0, len(b.stacks))
		for sid, cnt := range b.stacks {
			stackTotals[sid] += cnt
			bs = append(bs, bucketStackSnap{sid: sid, samples: cnt})
		}
		bucketSnaps = append(bucketSnaps, bucketSnap{
			pb: &pb.ProfileBucket{
				T:             b.t,
				Samples:       b.samples,
				KernelSamples: b.kernel,
				UserSamples:   b.user,
				TopFrame:      b.top(),
				DurationMs:    acc.bucketMs,
			},
			stacks: bs,
		})
	}
	type presentStack struct {
		sid     uint32
		key     flame.StackKey
		samples int64
	}
	present := make([]presentStack, 0, len(stackTotals))
	for sid, cnt := range stackTotals {
		present = append(present, presentStack{
			sid:     sid,
			key:     acc.stackList[sid],
			samples: cnt,
		})
	}
	s.mu.RUnlock()

	raw.Stacks = make([]*pb.FoldedStack, len(present))
	sidToRaw := make(map[uint32]uint32, len(present))
	for i, ps := range present {
		sidToRaw[ps.sid] = uint32(i)
		raw.Stacks[i] = &pb.FoldedStack{
			Frames:   flame.IDs(ps.key.Frames),
			Samples:  ps.samples,
			LeafType: ps.key.LeafType,
			Comm:     ps.key.Comm,
		}
	}
	slices.SortFunc(bucketSnaps, func(a, b bucketSnap) int { return cmp.Compare(a.pb.T, b.pb.T) })
	raw.Buckets = make([]*pb.ProfileBucket, len(bucketSnaps))
	for i, bs := range bucketSnaps {
		if len(bs.stacks) > 0 {
			bs.pb.StackIndexes = make([]uint32, len(bs.stacks))
			bs.pb.StackSamples = make([]int64, len(bs.stacks))
			for j, st := range bs.stacks {
				bs.pb.StackIndexes[j] = sidToRaw[st.sid]
				bs.pb.StackSamples[j] = st.samples
			}
		}
		raw.Buckets[i] = bs.pb
	}
	return summarize(raw, req)
}

// QueryNode builds a filtered host-wide profile from the node's ring buffer.
// Time-range filtering (from_ms / to_ms) filters both buckets and stacks.
func (s *Store) QueryNode(node string, req *pb.GetProfileRequest, status *pb.ProfilerStatus) *pb.CommandProfile {
	fromMs := req.GetFromMs()
	toMs := req.GetToMs()

	s.mu.RLock()
	raw := &pb.CommandProfile{
		Node:       node,
		Profiler:   EngineName,
		Live:       true,
		Active:     status.GetActive(),
		StatusNote: status.GetError(),
		SampleHz:   s.sampleHz,
	}
	stacks := flame.Stacks{}
	comms := map[string]bool{}
	types := map[string]bool{}
	for _, b := range s.nodeBuckets {
		if fromMs > 0 && b.t+bucketDurationMs <= fromMs {
			continue
		}
		if toMs > 0 && b.t > toMs {
			continue
		}
		if raw.StartedAt == 0 || b.startedAt < raw.StartedAt {
			raw.StartedAt = b.startedAt
		}
		raw.UpdatedAt = max(raw.UpdatedAt, b.updatedAt)
		raw.TotalSamples += b.samples
		raw.KernelSamples += b.kernel
		raw.UserSamples += b.user
		for sk, cnt := range b.stacks {
			stacks[sk] += cnt
		}
		for c := range b.comms {
			comms[c] = true
		}
		for ft := range b.types {
			types[ft] = true
		}
		raw.Buckets = append(raw.Buckets, &pb.ProfileBucket{
			T:             b.t,
			Samples:       b.samples,
			KernelSamples: b.kernel,
			UserSamples:   b.user,
			TopFrame:      b.top(),
			DurationMs:    bucketDurationMs,
		})
	}
	if len(raw.Buckets) == 0 {
		s.mu.RUnlock()
		return raw
	}
	// Frames are never changed once recorded, so the copy shares them.
	raw.Frames = slices.Clone(s.nodeFrames.Frames())
	s.mu.RUnlock()

	sort.Slice(raw.Buckets, func(i, j int) bool { return raw.Buckets[i].T < raw.Buckets[j].T })
	raw.Comms, raw.FrameTypes = sortedKeys(comms), sortedKeys(types)
	raw.Stacks = stacks.Folded()
	return summarize(raw, req)
}

// QueryProfile applies GetProfileRequest filters to an already-persisted
// CommandProfile (e.g. loaded from controller SQLite for a finished command).
func QueryProfile(raw *pb.CommandProfile, req *pb.GetProfileRequest) *pb.CommandProfile {
	if raw == nil {
		return nil
	}
	return summarize(raw, req)
}

// MergeProfiles combines a host node profile with zero or more active guest VM
// profiles (each tagged by its Node label, e.g. "vm:<wsID>") into a single
// node-wide profile, then applies req's filters and ranking. Each guest stack
// is rooted under a synthetic process frame carrying the guest's Node label so
// flamegraphs and top-function views distinguish guest work from host processes.
func MergeProfiles(node string, host *pb.CommandProfile, guests []*pb.CommandProfile, req *pb.GetProfileRequest) *pb.CommandProfile {
	if len(guests) == 0 {
		if host == nil {
			return &pb.CommandProfile{Node: node, Profiler: EngineName, Live: true}
		}
		return summarize(host, req)
	}

	var frames flame.Table
	merged := &pb.CommandProfile{
		Node:     node,
		Profiler: EngineName,
		Live:     true,
	}
	if host != nil {
		merged.Active = host.Active
		merged.StatusNote = host.StatusNote
		merged.SampleHz = host.SampleHz
	}

	type mergedBucket struct {
		t, durationMs int64
		samples       int64
		kernel        int64
		user          int64
		topFrame      uint32
		topSamples    int64
	}
	buckets := map[int64]*mergedBucket{}
	stacks := flame.Stacks{}
	comms := map[string]bool{}
	types := map[string]bool{}

	absorb := func(src *pb.CommandProfile, vmRootLabel string) {
		if src == nil {
			return
		}
		if src.Active {
			merged.Active = true
			if merged.StatusNote != "" && host != nil && !host.Active {
				merged.StatusNote = ""
			}
		}
		if merged.SampleHz == 0 && src.SampleHz > 0 {
			merged.SampleHz = src.SampleHz
		}
		if src.StartedAt > 0 && (merged.StartedAt == 0 || src.StartedAt < merged.StartedAt) {
			merged.StartedAt = src.StartedAt
		}
		merged.UpdatedAt = max(merged.UpdatedAt, src.UpdatedAt)
		merged.TotalSamples += src.TotalSamples
		merged.KernelSamples += src.KernelSamples
		merged.UserSamples += src.UserSamples
		for _, c := range src.Comms {
			if c != "" {
				comms[c] = true
			}
		}
		for _, ft := range src.FrameTypes {
			if ft != "" {
				types[ft] = true
			}
		}

		// Remap src's frame indices into the unified table.
		remap := make([]uint32, len(src.Frames))
		for i, f := range src.Frames {
			remap[i] = frames.ID(f)
		}
		var rootID uint32
		hasRoot := vmRootLabel != ""
		if hasRoot {
			rootID = frames.ID(&pb.ProfileFrame{Name: vmRootLabel, FrameType: "process"})
			types["process"] = true
		}

		for _, st := range src.Stacks {
			if st.Samples <= 0 || len(st.Frames) == 0 {
				continue
			}
			ids := make([]byte, 0, 4*(len(st.Frames)+1))
			if hasRoot {
				ids = flame.AppendID(ids, rootID)
			}
			for _, oldID := range st.Frames {
				if int(oldID) < len(remap) {
					ids = flame.AppendID(ids, remap[oldID])
				}
			}
			if len(ids) == 0 {
				continue
			}
			sk := flame.StackKey{Frames: string(ids), LeafType: st.LeafType, Comm: st.Comm}
			stacks[sk] += st.Samples
		}

		for _, b := range src.Buckets {
			mb := buckets[b.T]
			if mb == nil {
				dur := b.DurationMs
				if dur <= 0 {
					dur = bucketDurationMs
				}
				mb = &mergedBucket{t: b.T, durationMs: dur, topFrame: noTopFrame}
				buckets[b.T] = mb
			}
			mb.samples += b.Samples
			mb.kernel += b.KernelSamples
			mb.user += b.UserSamples
			if b.TopFrame != noTopFrame && int(b.TopFrame) < len(remap) && b.Samples > mb.topSamples {
				mb.topFrame = remap[b.TopFrame]
				mb.topSamples = b.Samples
			}
		}
	}

	absorb(host, "")
	for _, gp := range guests {
		if gp == nil || gp.TotalSamples == 0 {
			continue
		}
		label := gp.Node
		if label == "" {
			label = "vm"
		}
		absorb(gp, label)
	}

	merged.Frames = frames.Frames()
	merged.Comms = sortedKeys(comms)
	merged.FrameTypes = sortedKeys(types)
	merged.Stacks = stacks.Folded()
	merged.Buckets = make([]*pb.ProfileBucket, 0, len(buckets))
	for _, mb := range buckets {
		merged.Buckets = append(merged.Buckets, &pb.ProfileBucket{
			T:             mb.t,
			Samples:       mb.samples,
			KernelSamples: mb.kernel,
			UserSamples:   mb.user,
			TopFrame:      mb.topFrame,
			DurationMs:    mb.durationMs,
		})
	}
	sort.Slice(merged.Buckets, func(i, j int) bool { return merged.Buckets[i].T < merged.Buckets[j].T })
	return summarize(merged, req)
}

// summarize filters raw's stacks and buckets as req asks, ranks the stacks
// that match and the functions in them, and returns the top ones, with a
// frames table of only the frames they name. Stacks give way, the least
// sampled first, until it fits MaxProfileBytes; the timeline is bounded
// where it is recorded (maxCmdBuckets, maxNodeBuckets).
func summarize(raw *pb.CommandProfile, req *pb.GetProfileRequest) *pb.CommandProfile {
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
	hasBucketStacks := false
	for _, b := range raw.Buckets {
		if len(b.StackIndexes) > 0 && len(b.StackSamples) > 0 {
			hasBucketStacks = true
		}
		if fromMs > 0 && b.T+b.DurationMs <= fromMs {
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

	// Build the candidate stacks for this query. When a time window is active
	// and the profile carries per-bucket stack counts, reconstruct stack counts
	// strictly from the in-window buckets so persisted profiles filter by time
	// window just like live ones.
	var candidates []indexedStack
	if (fromMs > 0 || toMs > 0) && hasBucketStacks {
		windowCounts := make(map[uint32]int64)
		for _, b := range buckets {
			m := min(len(b.StackIndexes), len(b.StackSamples))
			for i := 0; i < m; i++ {
				idx := b.StackIndexes[i]
				cnt := b.StackSamples[i]
				if int(idx) < len(raw.Stacks) && cnt > 0 {
					windowCounts[idx] += cnt
				}
			}
		}
		candidates = make([]indexedStack, 0, len(windowCounts))
		for rawIdx, st := range raw.Stacks {
			if cnt := windowCounts[uint32(rawIdx)]; cnt > 0 {
				candidates = append(candidates, indexedStack{
					rawIdx: uint32(rawIdx),
					stack: &pb.FoldedStack{
						Frames:   st.Frames,
						Samples:  cnt,
						LeafType: st.LeafType,
						Comm:     st.Comm,
					},
				})
			}
		}
	} else if (fromMs > 0 || toMs > 0) && len(raw.Buckets) > 0 && len(buckets) == 0 {
		// Empty time window selects no buckets and no stacks.
	} else {
		candidates = make([]indexedStack, len(raw.Stacks))
		for rawIdx, st := range raw.Stacks {
			candidates[rawIdx] = indexedStack{rawIdx: uint32(rawIdx), stack: st}
		}
	}

	// Which frames the query and the type filter take, decided once a frame.
	queried := make([]bool, len(raw.Frames))
	typed := make([]bool, len(raw.Frames))
	for i, f := range raw.Frames {
		queried[i] = q != "" && (strings.Contains(strings.ToLower(flame.Label(f)), q) || strings.Contains(strings.ToLower(f.Module), q))
		typed[i] = ftFilter != "" && strings.ToLower(f.FrameType) == ftFilter
	}
	anyFrame := func(st *pb.FoldedStack, take []bool) bool {
		return slices.ContainsFunc(st.Frames, func(id uint32) bool { return take[id] })
	}

	var matchedStacks []indexedStack
	var matchedSamples, matchedKernel, matchedUser int64
	var fns flame.Functions
	for _, cand := range candidates {
		st := cand.stack
		if commFilter != "" && st.Comm != commFilter {
			continue
		}
		if ftFilter != "" && strings.ToLower(st.LeafType) != ftFilter && !anyFrame(st, typed) {
			continue
		}
		if q != "" && !anyFrame(st, queried) && !strings.Contains(strings.ToLower(st.Comm), q) {
			continue
		}
		matchedStacks = append(matchedStacks, cand)
		matchedSamples += st.Samples
		if st.LeafType == "kernel" {
			matchedKernel += st.Samples
		} else {
			matchedUser += st.Samples
		}
		fns.Add(st.Frames, st.Samples)
	}

	sort.Slice(matchedStacks, func(i, j int) bool {
		a, b := matchedStacks[i].stack, matchedStacks[j].stack
		if a.Samples != b.Samples {
			return a.Samples > b.Samples
		}
		if c := slices.Compare(a.Frames, b.Frames); c != 0 {
			return c < 0
		}
		return a.Comm < b.Comm
	})

	topFns := fns.Top(raw.Frames, matchedSamples)
	topFns = topFns[:min(len(topFns), limit, maxSnapshotFuncs)]

	out := &pb.CommandProfile{
		CommandId:      raw.CommandId,
		Node:           raw.Node,
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
	}
	// If only a time-window filter was applied without stack filters, reflect bucket totals.
	if (fromMs > 0 || toMs > 0) && q == "" && ftFilter == "" && commFilter == "" && bucketSamples > 0 {
		out.MatchedSamples = bucketSamples
		out.KernelSamples = bucketKernel
		out.UserSamples = bucketUser
	}
	for n := min(len(matchedStacks), limit); ; n /= 2 {
		keepTop(out, raw.Frames, matchedStacks[:n], topFns, buckets)
		if n == 0 || proto.Size(out) <= MaxProfileBytes {
			return out
		}
	}
}

type indexedStack struct {
	rawIdx uint32
	stack  *pb.FoldedStack
}

// keepTop sets out's stacks, functions and buckets to copies of these,
// which name frames in from, naming them instead in a frames table of out's
// own that lists only the frames they name. Per-bucket stack references are
// remapped to the surviving indices in out.Stacks.
func keepTop(out *pb.CommandProfile, from []*pb.ProfileFrame, stacks []indexedStack, fns []*pb.ProfileFunction, buckets []*pb.ProfileBucket) {
	var t flame.Table
	out.Stacks = make([]*pb.FoldedStack, len(stacks))
	oldToNew := make(map[uint32]uint32, len(stacks))
	for i, e := range stacks {
		out.Stacks[i] = &pb.FoldedStack{Frames: slices.Clone(e.stack.Frames), Samples: e.stack.Samples, LeafType: e.stack.LeafType, Comm: e.stack.Comm}
		oldToNew[e.rawIdx] = uint32(i)
	}
	out.TopFunctions = make([]*pb.ProfileFunction, len(fns))
	for i, fn := range fns {
		out.TopFunctions[i] = proto.CloneOf(fn)
	}
	t.Rebase(from, out.Stacks, out.TopFunctions)
	out.Buckets = make([]*pb.ProfileBucket, len(buckets))
	type bucketRef struct {
		idx     uint32
		samples int64
	}
	var refs []bucketRef
	for i, b := range buckets {
		nb := &pb.ProfileBucket{
			T:             b.T,
			Samples:       b.Samples,
			KernelSamples: b.KernelSamples,
			UserSamples:   b.UserSamples,
			TopFrame:      noTopFrame,
			DurationMs:    b.DurationMs,
		}
		if b.TopFrame != noTopFrame && int(b.TopFrame) < len(from) {
			nb.TopFrame = t.ID(from[b.TopFrame])
		}
		if m := min(len(b.StackIndexes), len(b.StackSamples)); m > 0 && len(oldToNew) > 0 {
			refs = refs[:0]
			for j := 0; j < m; j++ {
				if newIdx, ok := oldToNew[b.StackIndexes[j]]; ok && b.StackSamples[j] > 0 {
					refs = append(refs, bucketRef{idx: newIdx, samples: b.StackSamples[j]})
				}
			}
			if len(refs) > 0 {
				slices.SortFunc(refs, func(a, b bucketRef) int { return cmp.Compare(a.idx, b.idx) })
				nb.StackIndexes = make([]uint32, len(refs))
				nb.StackSamples = make([]int64, len(refs))
				for j, r := range refs {
					nb.StackIndexes[j] = r.idx
					nb.StackSamples[j] = r.samples
				}
			}
		}
		out.Buckets[i] = nb
	}
	out.Frames = t.Frames()
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
