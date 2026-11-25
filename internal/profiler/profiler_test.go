package profiler

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func folded(p *pb.CommandProfile) []string {
	var out []string
	for _, st := range p.Stacks {
		out = append(out, fmt.Sprintf("%s %d", flame.Fold(p.Frames, st), st.Samples))
	}
	return out
}

// javaFrame is a frame named the way a JVM names one: a fully qualified
// signature some 300 characters long.
func javaFrame(i int) ResolvedFrame {
	return ResolvedFrame{
		Name:      fmt.Sprintf("tlc2.tool.TLCState tlc2.tool.impl.Tool.getNextStates%d(tlc2.tool.Action, tla2sany.semantic.OpApplNode, tlc2.tool.impl.ActionItemList, tlc2.util.Context, tlc2.tool.TLCState, tlc2.tool.TLCState, tlc2.tool.INextStateFunctor, tlc2.tool.coverage.CostModel, tla2sany.semantic.ExprOrOpArgNode[], int, int)", i),
		Module:    "tla2tools.jar",
		FrameType: "hotspot",
	}
}

// recordJava records stacks as deep as TLC's (150 frames), each sampled
// count(i) times, the frames of stack i drawn by frame(i, depth).
func recordJava(s *Store, stacks int, frame func(stack, depth int) ResolvedFrame, count func(stack int) int) {
	t0 := time.UnixMilli(1_795_300_000_000)
	for i := range stacks {
		frames := make([]ResolvedFrame, 150)
		for d := range frames {
			frames[d] = frame(i, d)
		}
		for range count(i) {
			s.Record(Sample{CommandID: "tlc0000000", Timestamp: t0.Add(time.Duration(i) * time.Millisecond), Comm: "java", Frames: frames})
		}
	}
}

func TestExtractCommandID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0::/hotbed.slice/hotbed-2plyu4zxok.service\n", "2plyu4zxok"},
		{"0::/user.slice/user-hotbed.slice/hotbed-abc1234567.service\n", "abc1234567"},
		{"0::/hotbed_bench.slice/hotbed-xyz9876543.service/sub\n", "xyz9876543"},
		{"0::/hotbed/2plyu4zxok\n", "2plyu4zxok"},
		{"0::/hotbed/cmd-vm-1/sub\n", "cmd-vm-1"},
		{"0::/system.slice/sshd.service\n", ""},
	}
	for _, tc := range cases {
		if got := ExtractCommandIDFromCgroup(tc.in); got != tc.want {
			t.Errorf("ExtractCommandIDFromCgroup(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := ExtractCommandIDFromEnviron([]byte("PATH=/bin\x00HOTBED_COMMAND=2plyu4zxok\x00USER=ramon\x00")); got != "2plyu4zxok" {
		t.Fatalf("ExtractCommandIDFromEnviron = %q", got)
	}
}

func TestStoreRecordQueryAndPersist(t *testing.T) {
	s := NewStore(20)
	t0 := time.UnixMilli(1_795_300_000_000)

	// Record recursive Go stack + kernel stack across two 5-second buckets.
	for i := 0; i < 6; i++ {
		s.Record(Sample{
			CommandID: "cmd1234567",
			Timestamp: t0.Add(time.Duration(i) * time.Second),
			Comm:      "compile",
			Frames: []ResolvedFrame{
				{Name: "main.main", Module: "compile", FrameType: "go"},
				{Name: "ssa.walk", Module: "compile", FrameType: "go"},
				{Name: "ssa.walk", Module: "compile", FrameType: "go"}, // recursive frame: should count once in total
				{Name: "ssa.regalloc", Module: "compile", FrameType: "go"},
			},
		})
	}
	for i := 0; i < 4; i++ {
		s.Record(Sample{
			CommandID: "cmd1234567",
			Timestamp: t0.Add(10 * time.Second),
			Comm:      "worker.test",
			Frames: []ResolvedFrame{
				{Name: "testing.tRunner", Module: "worker.test", FrameType: "go"},
				{Name: "vfs_write", Module: "vmlinux", FrameType: "kernel"},
			},
		})
	}

	status := &pb.ProfilerStatus{Active: true, Engine: EngineName, SampleHz: 20, TotalSamples: s.TotalSamples()}
	all := s.Query("cmd1234567", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd1234567"}}, true, status)
	if all.TotalSamples != 10 || all.MatchedSamples != 10 || all.UserSamples != 6 || all.KernelSamples != 4 {
		t.Fatalf("unexpected counts: total=%d matched=%d user=%d kernel=%d", all.TotalSamples, all.MatchedSamples, all.UserSamples, all.KernelSamples)
	}
	if len(all.Buckets) != 3 {
		t.Fatalf("expected 3 time buckets, got %d", len(all.Buckets))
	}
	// Verify recursive ssa.walk wasn't double-counted in total_samples (should be 6, not 12).
	var top []string
	for _, fn := range all.TopFunctions {
		top = append(top, fmt.Sprintf("%s %d/%d", all.Frames[fn.Frame].Name, fn.SelfSamples, fn.TotalSamples))
	}
	if want := []string{"ssa.regalloc 6/6", "vfs_write 4/4", "main.main 0/6", "ssa.walk 0/6", "testing.tRunner 0/4"}; !slices.Equal(top, want) {
		t.Fatalf("top functions = %q, want %q", top, want)
	}
	if got, want := folded(all), []string{"main.main;ssa.walk;ssa.walk;ssa.regalloc 6", "testing.tRunner;vfs_write_[k] 4"}; !slices.Equal(got, want) {
		t.Fatalf("stacks = %q, want %q", got, want)
	}
	for i, b := range all.Buckets {
		if got := all.Frames[b.TopFrame].Name; got != []string{"ssa.regalloc", "ssa.regalloc", "vfs_write"}[i] {
			t.Fatalf("bucket %d top frame = %q", i, got)
		}
	}

	// Filter by query + frameType + comm.
	filtered := s.Query("cmd1234567", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd1234567"}, Query: "vfs_write", FrameType: "kernel", Comm: "worker.test"}, true, status)
	if filtered.MatchedSamples != 4 || len(filtered.Stacks) != 1 || filtered.Stacks[0].LeafType != "kernel" {
		t.Fatalf("unexpected filtered profile: %+v", filtered)
	}
	// A reply lists only the frames it names.
	if len(filtered.Frames) != 3 {
		t.Fatalf("filtered frames = %v, want the stack's two and the other buckets' top", filtered.Frames)
	}

	// Persist snapshot to disk and query via QueryProfile (finished command path).
	snap := s.Snapshot("cmd1234567", status)
	path := filepath.Join(t.TempDir(), "cmd1234567.pb.gz")
	if err := SaveProfile(path, snap); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	loaded, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	reFiltered := QueryProfile(loaded, &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd1234567"}, Query: "regalloc"})
	if reFiltered.MatchedSamples != 6 || len(reFiltered.Stacks) != 1 {
		t.Fatalf("unexpected reFiltered profile: %+v", reFiltered)
	}
}

func TestSymbolizerSelfELF(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("self binary is Mach-O on non-Linux; ELF test runs on Linux")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatalf("open self: %v", err)
	}
	defer f.Close()

	sym := NewSymbolizer(8)
	sym.IndexELF("self-id", f)
	if !sym.Has("self-id") {
		t.Fatalf("expected self-id to be indexed")
	}
	// Resolve the PC of this test function itself!
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// Note: ASLR/PIE bias means runtime PC may differ from static ELF VA, so also verify non-empty table.
	sym.mu.RLock()
	bs := sym.entries["self-id"]
	sym.mu.RUnlock()
	if bs == nil || !bs.isGo || bs.goTab == nil {
		t.Fatalf("expected Go pclntab in Linux test binary, got %+v (pc=%#x)", bs, pc)
	}
}

func TestDemangleSymbol(t *testing.T) {
	// Rust/C++ mangled name should demangle cleanly; plain names stay unchanged.
	if got := demangleSymbol("_ZN4core3ptr13drop_in_place17h1234567890abcdefE"); got == "_ZN4core3ptr13drop_in_place17h1234567890abcdefE" {
		t.Fatalf("expected demangled Rust/C++ symbol, got %q", got)
	}
	if got := demangleSymbol("main.work"); got != "main.work" {
		t.Fatalf("expected plain Go symbol unchanged, got %q", got)
	}
}

// A profile of deep stacks of long names, which reused names spell out
// 40 KB a stack, fits a message with every stack asked for: frames are
// listed once (board 7115).
func TestProfileOfDeepStacksListsFramesOnce(t *testing.T) {
	s := NewStore(20)
	// 400 distinct frames make 4000 distinct stacks, told apart by their
	// first two frames.
	recordJava(s, maxStacksPerCmd, func(i, d int) ResolvedFrame {
		switch d {
		case 0:
			return javaFrame(i % 400)
		case 1:
			return javaFrame(i / 400)
		}
		return javaFrame((i + d*13) % 400)
	}, func(i int) int { return 1 + i%5 })
	status := &pb.ProfilerStatus{Active: true}

	snap := s.Snapshot("tlc0000000", status)
	if len(snap.Stacks) != maxSnapshotStack {
		t.Fatalf("snapshot has %d stacks, want %d", len(snap.Stacks), maxSnapshotStack)
	}
	if size := proto.Size(snap); size > 512<<10 {
		t.Fatalf("snapshot of %d stacks is %d bytes, want at most 512 KiB", len(snap.Stacks), size)
	}
	if got := len(strings.Split(flame.Fold(snap.Frames, snap.Stacks[0]), ";")); got != 150 {
		t.Fatalf("first stack has %d frames, want 150", got)
	}

	reply := s.Query("tlc0000000", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "tlc0000000"}}, true, status)
	if len(reply.Stacks) != 200 || len(reply.TopFunctions) != 200 {
		t.Fatalf("reply has %d stacks and %d functions, want 200 of each", len(reply.Stacks), len(reply.TopFunctions))
	}
}

// When even listed once the frames of the stacks asked for would outgrow
// MaxProfileBytes, the least sampled stacks give way.
func TestProfileOverBudgetDropsLeastSampledStacks(t *testing.T) {
	s := NewStore(20)
	// Every frame of every stack distinct: 45 KB a stack, however listed.
	recordJava(s, 300, func(i, d int) ResolvedFrame { return javaFrame(i*150 + d) }, func(i int) int { return 300 - i })
	status := &pb.ProfilerStatus{Active: true}

	snap := s.Snapshot("tlc0000000", status)
	if size := proto.Size(snap); size > MaxProfileBytes {
		t.Fatalf("snapshot is %d bytes, over %d", size, MaxProfileBytes)
	}
	// Halving 300 stacks: 150, 75, 37, 18 (820 KB) fits.
	if len(snap.Stacks) != 18 {
		t.Fatalf("snapshot kept %d stacks, want 18", len(snap.Stacks))
	}
	for i, st := range snap.Stacks {
		if st.Samples != int64(300-i) {
			t.Fatalf("stack %d has %d samples, want the %dth most sampled (%d)", i, st.Samples, i+1, 300-i)
		}
	}
	if want := int64(300 * 301 / 2); snap.MatchedSamples != want || snap.TotalSamples != want {
		t.Fatalf("matched %d of %d samples, want all %d", snap.MatchedSamples, snap.TotalSamples, want)
	}
}

func TestStoreNodeRingBufferAndWindowFiltering(t *testing.T) {
	s := NewStore(20)
	s.maxNodeBuckets = 3 // keep only the 3 newest 5-second buckets
	t0 := time.UnixMilli(1_795_300_000_000)

	// Bucket 0 (t0): host-only process (no CommandID), will be evicted once bucket 3 arrives.
	s.Record(Sample{
		Timestamp: t0,
		Comm:      "old-daemon",
		Frames: []ResolvedFrame{
			{Name: "daemon.loop", Module: "old-daemon", FrameType: "native"},
		},
	})
	// Bucket 1 (t0 + 5s): host-only kernel/daemon sample (no CommandID).
	for i := 0; i < 3; i++ {
		s.Record(Sample{
			Timestamp: t0.Add(5 * time.Second),
			Comm:      "tailscaled",
			Frames: []ResolvedFrame{
				{Name: "net.Read", Module: "tailscaled", FrameType: "go"},
				{Name: "tcp_recvmsg", Module: "vmlinux", FrameType: "kernel"},
			},
		})
	}
	// Bucket 2 (t0 + 10s): command sample (also recorded in node ring buffer).
	for i := 0; i < 5; i++ {
		s.Record(Sample{
			CommandID: "cmd1234567",
			Timestamp: t0.Add(10 * time.Second),
			Comm:      "rustc",
			Frames: []ResolvedFrame{
				{Name: "rustc_driver::main", Module: "rustc", FrameType: "native"},
				{Name: "llvm::Pass::run", Module: "rustc", FrameType: "native"},
			},
		})
	}
	// Bucket 3 (t0 + 15s): triggers eviction of Bucket 0.
	for i := 0; i < 2; i++ {
		s.Record(Sample{
			Timestamp: t0.Add(15 * time.Second),
			Comm:      "btrfs-transacti",
			Frames: []ResolvedFrame{
				{Name: "btrfs_commit_transaction", Module: "btrfs", FrameType: "kernel"},
			},
		})
	}
	// Stale sample older than the ring buffer should be ignored without evicting live buckets.
	s.Record(Sample{
		Timestamp: t0.Add(-10 * time.Second),
		Comm:      "ancient",
		Frames:    []ResolvedFrame{{Name: "ancient.fn", FrameType: "native"}},
	})

	status := &pb.ProfilerStatus{Active: true, Engine: EngineName, SampleHz: 20, TotalSamples: s.TotalSamples()}
	all := s.QueryNode("asahimbp", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "asahimbp"}}, status)
	if all.Node != "asahimbp" || !all.Live || !all.Active {
		t.Fatalf("unexpected node profile header: %+v", all)
	}
	// Bucket 0 (1 sample) evicted, ancient ignored -> 3 + 5 + 2 = 10 samples across 3 buckets.
	if all.TotalSamples != 10 || all.MatchedSamples != 10 || all.UserSamples != 5 || all.KernelSamples != 5 {
		t.Fatalf("unexpected node counts: total=%d matched=%d user=%d kernel=%d",
			all.TotalSamples, all.MatchedSamples, all.UserSamples, all.KernelSamples)
	}
	if len(all.Buckets) != 3 {
		t.Fatalf("expected 3 retained buckets, got %d", len(all.Buckets))
	}
	// Verify process comm is prepended as root frame and evicted old-daemon is gone.
	foundRustc := false
	for _, st := range all.Stacks {
		if st.Comm == "old-daemon" || st.Comm == "ancient" {
			t.Fatalf("evicted/stale sample leaked into node profile: %+v", st)
		}
		if st.Comm == "rustc" && flame.Fold(all.Frames, st) == "rustc;rustc_driver::main;llvm::Pass::run" && st.Samples == 5 {
			foundRustc = true
		}
	}
	if !foundRustc {
		t.Fatalf("expected rustc root-prefixed stack in node profile, got %+v", all.Stacks)
	}

	// Exact 5s time-window query: only Bucket 2 (t0 + 10s .. t0 + 14.999s) should match BOTH buckets and stacks!
	winStart := t0.Add(10 * time.Second).UnixMilli()
	win := s.QueryNode("asahimbp", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "asahimbp"},
		FromMs: winStart,
		ToMs:   winStart + bucketDurationMs - 1,
	}, status)
	if win.TotalSamples != 5 || win.MatchedSamples != 5 || len(win.Buckets) != 1 || len(win.Stacks) != 1 || win.Stacks[0].Comm != "rustc" {
		t.Fatalf("expected exact 5s window to return only rustc bucket, got total=%d matched=%d buckets=%d stacks=%+v",
			win.TotalSamples, win.MatchedSamples, len(win.Buckets), win.Stacks)
	}

	// Per-command query still isolates cmd1234567 and excludes host-only samples.
	cmdProf := s.Query("cmd1234567", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "cmd1234567"}}, true, status)
	if cmdProf.TotalSamples != 5 || cmdProf.MatchedSamples != 5 {
		t.Fatalf("command profile should only have cmd1234567 samples, got total=%d matched=%d", cmdProf.TotalSamples, cmdProf.MatchedSamples)
	}
}

// The node's frames table keeps only what its buckets still name: frames of
// buckets the ring let go are dropped when it compacts, and the stacks it
// keeps read as they did.
func TestNodeFramesCompactToWhatBucketsName(t *testing.T) {
	s := NewStore(20)
	s.maxNodeBuckets, s.maxNodeFrames = 2, 8
	t0 := time.UnixMilli(1_795_300_000_000)
	for b := range 6 {
		for i := range 3 {
			s.Record(Sample{
				Timestamp: t0.Add(time.Duration(b) * bucketDurationMs * time.Millisecond),
				Comm:      "app",
				Frames: []ResolvedFrame{
					{Name: "main", FrameType: "go"},
					{Name: fmt.Sprintf("work%d_%d", b, i), FrameType: "go"},
				},
			})
		}
	}
	// Each bucket adds 3 frames to app and main: compacting at over 8 keeps
	// the last two buckets' 6 and what came after.
	if n := len(s.nodeFrames.Frames()); n > 2*8 {
		t.Fatalf("node frames table has %d frames, want it compacted", n)
	}
	p := s.QueryNode("n", &pb.GetProfileRequest{}, &pb.ProfilerStatus{Active: true})
	got := folded(p)
	slices.Sort(got)
	if want := []string{"app;main;work4_0 1", "app;main;work4_1 1", "app;main;work4_2 1", "app;main;work5_0 1", "app;main;work5_1 1", "app;main;work5_2 1"}; !slices.Equal(got, want) {
		t.Fatalf("node stacks = %q, want %q", got, want)
	}
	for _, b := range p.Buckets {
		if name := p.Frames[b.TopFrame].Name; !strings.HasPrefix(name, "work") {
			t.Fatalf("bucket top = %q", name)
		}
	}
}

// A command sampled for days keeps a timeline of at most maxCmdBuckets:
// its buckets double in length as it grows, each saying how long it is,
// with every sample and the right top frame kept (board 7222).
func TestLongCommandsTimelineStaysBounded(t *testing.T) {
	s := NewStore(20)
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)
	const days = 3
	n := 0
	for at := time.Duration(0); at < days*24*time.Hour; at += bucketDurationMs * time.Millisecond {
		leaf := "steady"
		if at >= 24*time.Hour && at < 24*time.Hour+10*time.Minute {
			leaf = "burst" // the second day starts with ten minutes of something else
		}
		s.Record(Sample{CommandID: "longcmd000", Timestamp: t0.Add(at), Comm: "app", Frames: []ResolvedFrame{{Name: "main", FrameType: "go"}, {Name: leaf, FrameType: "go"}}})
		n++
	}
	p := s.Query("longcmd000", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "longcmd000"}}, true, &pb.ProfilerStatus{Active: true})
	// 51840 five-second buckets halve, seven times, to 640 seconds: 405 of
	// them, or 406 where t0 falls inside one.
	first, last := t0.UnixMilli()/640_000, (t0.Add(days*24*time.Hour).UnixMilli()-1)/640_000
	if want := int(last - first + 1); len(p.Buckets) != want {
		t.Fatalf("%d buckets, want %d", len(p.Buckets), want)
	}
	var total int64
	for i, b := range p.Buckets {
		if b.DurationMs != 640_000 || b.T%640_000 != 0 {
			t.Fatalf("bucket %d: t=%d, %d ms long; want 640 s long and aligned", i, b.T, b.DurationMs)
		}
		total += b.Samples
	}
	if total != int64(n) || p.TotalSamples != int64(n) {
		t.Fatalf("buckets hold %d samples of %d (total %d)", total, n, p.TotalSamples)
	}
	burst := slices.IndexFunc(p.Buckets, func(b *pb.ProfileBucket) bool { return p.Frames[b.TopFrame].Name == "burst" })
	if burst < 0 || p.Buckets[burst].T > t0.Add(24*time.Hour).UnixMilli() || p.Buckets[burst].T+640_000 < t0.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("no bucket topped by burst where the second day starts (index %d)", burst)
	}
	if size := proto.Size(p); size > 64<<10 {
		t.Fatalf("profile of a 3-day command is %d bytes, want under 64 KiB", size)
	}

	// A window of one bucket takes that bucket.
	b := p.Buckets[burst]
	win := s.Query("longcmd000", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "longcmd000"}, FromMs: b.T, ToMs: b.T + b.DurationMs - 1}, true, &pb.ProfilerStatus{Active: true})
	if len(win.Buckets) != 1 || win.Buckets[0].T != b.T || win.MatchedSamples != b.Samples {
		t.Fatalf("window of one bucket: %d buckets, matched %d; want 1 and %d", len(win.Buckets), win.MatchedSamples, b.Samples)
	}
	if len(win.TopFunctions) == 0 || win.Frames[win.TopFunctions[0].Frame].Name != "burst" {
		t.Fatalf("burst bucket top function = %v, want burst", win.TopFunctions)
	}
	for _, st := range win.Stacks {
		if st.Samples > b.Samples {
			t.Fatalf("lifetime stack leaked into single-bucket window: %s (%d > %d)", flame.Fold(win.Frames, st), st.Samples, b.Samples)
		}
	}
}

// Regression test for board 7882: Query(cmdID, fromMs, toMs, ...) must filter
// Stacks, TopFunctions, and MatchedSamples/KernelSamples/UserSamples strictly
// to the matching time buckets, never leaking lifetime stacks.
func TestStoreCommandQueryTimeWindowFiltering(t *testing.T) {
	s := NewStore(20)
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)

	// Bucket 0 [t0, t0+5s): 7 user samples in phaseA (comm "compiler").
	for i := range 7 {
		s.Record(Sample{
			CommandID: "win0000001",
			Timestamp: t0.Add(time.Duration(i*100) * time.Millisecond),
			Comm:      "compiler",
			Frames: []ResolvedFrame{
				{Name: "main.main", Module: "compiler", FrameType: "go"},
				{Name: "phaseA.run", Module: "compiler", FrameType: "go"},
			},
		})
	}
	// Bucket 1 [t0+5s, t0+10s): 3 kernel samples in phaseB (comm "linker").
	for i := range 3 {
		s.Record(Sample{
			CommandID: "win0000001",
			Timestamp: t0.Add(5*time.Second + time.Duration(i*100)*time.Millisecond),
			Comm:      "linker",
			Frames: []ResolvedFrame{
				{Name: "linker.write", Module: "linker", FrameType: "native"},
				{Name: "ext4_write", Module: "vmlinux", FrameType: "kernel"},
			},
		})
	}
	// Bucket 2 [t0+10s, t0+15s): 5 user samples in phaseC (comm "tester").
	for i := range 5 {
		s.Record(Sample{
			CommandID: "win0000001",
			Timestamp: t0.Add(10*time.Second + time.Duration(i*100)*time.Millisecond),
			Comm:      "tester",
			Frames: []ResolvedFrame{
				{Name: "testing.run", Module: "tester", FrameType: "go"},
				{Name: "phaseC.check", Module: "tester", FrameType: "go"},
			},
		})
	}

	status := &pb.ProfilerStatus{Active: true, Engine: EngineName, SampleHz: 20}

	// Narrow window covering only Bucket 1 [t0+5s, t0+10s).
	b1Start := t0.Add(5 * time.Second).UnixMilli()
	win1 := s.Query("win0000001", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "win0000001"},
		FromMs: b1Start,
		ToMs:   b1Start + bucketDurationMs - 1,
	}, true, status)

	if win1.TotalSamples != 15 {
		t.Fatalf("TotalSamples = %d, want 15", win1.TotalSamples)
	}
	if win1.MatchedSamples != 3 || win1.KernelSamples != 3 || win1.UserSamples != 0 {
		t.Fatalf("bucket 1 counts: matched=%d kernel=%d user=%d, want 3/3/0", win1.MatchedSamples, win1.KernelSamples, win1.UserSamples)
	}
	if len(win1.Buckets) != 1 || win1.Buckets[0].T != b1Start {
		t.Fatalf("bucket 1 buckets = %+v, want 1 bucket at %d", win1.Buckets, b1Start)
	}
	if got, want := folded(win1), []string{"linker.write;ext4_write_[k] 3"}; !slices.Equal(got, want) {
		t.Fatalf("bucket 1 stacks = %q, want %q (lifetime stacks leaked)", got, want)
	}
	var top1 []string
	for _, fn := range win1.TopFunctions {
		top1 = append(top1, fmt.Sprintf("%s %d/%d", win1.Frames[fn.Frame].Name, fn.SelfSamples, fn.TotalSamples))
	}
	if want := []string{"ext4_write 3/3", "linker.write 0/3"}; !slices.Equal(top1, want) {
		t.Fatalf("bucket 1 top functions = %q, want %q", top1, want)
	}

	// Two-bucket window [t0+5s, t0+15s): Bucket 1 + Bucket 2 only (no phaseA).
	win12 := s.Query("win0000001", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "win0000001"},
		FromMs: b1Start,
		ToMs:   t0.Add(15*time.Second).UnixMilli() - 1,
	}, true, status)
	if win12.MatchedSamples != 8 || win12.KernelSamples != 3 || win12.UserSamples != 5 {
		t.Fatalf("buckets 1..2 counts: matched=%d kernel=%d user=%d, want 8/3/5", win12.MatchedSamples, win12.KernelSamples, win12.UserSamples)
	}
	if got, want := folded(win12), []string{"testing.run;phaseC.check 5", "linker.write;ext4_write_[k] 3"}; !slices.Equal(got, want) {
		t.Fatalf("buckets 1..2 stacks = %q, want %q", got, want)
	}

	// Combining time window with query filter must not match frames outside the window.
	winMiss := s.Query("win0000001", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "win0000001"},
		FromMs: b1Start,
		ToMs:   b1Start + bucketDurationMs - 1,
		Query:  "phaseA",
	}, true, status)
	if winMiss.MatchedSamples != 0 || len(winMiss.Stacks) != 0 || len(winMiss.TopFunctions) != 0 {
		t.Fatalf("expected 0 matches for phaseA inside bucket 1, got matched=%d stacks=%v", winMiss.MatchedSamples, folded(winMiss))
	}

	// Empty time window returns 0 matched samples and no stacks.
	empty := s.Query("win0000001", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "win0000001"},
		FromMs: t0.Add(60 * time.Second).UnixMilli(),
		ToMs:   t0.Add(70 * time.Second).UnixMilli(),
	}, true, status)
	if empty.MatchedSamples != 0 || len(empty.Stacks) != 0 || len(empty.TopFunctions) != 0 || len(empty.Buckets) != 0 {
		t.Fatalf("expected empty window, got matched=%d stacks=%d buckets=%d", empty.MatchedSamples, len(empty.Stacks), len(empty.Buckets))
	}

	// Unbounded query (fromMs == 0 && toMs == 0) aggregates all 3 buckets.
	all := s.Query("win0000001", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "win0000001"}}, true, status)
	if all.MatchedSamples != 15 || all.KernelSamples != 3 || all.UserSamples != 12 || len(all.Stacks) != 3 {
		t.Fatalf("full query mismatch: matched=%d kernel=%d user=%d stacks=%d", all.MatchedSamples, all.KernelSamples, all.UserSamples, len(all.Stacks))
	}
}

// Regression test for board 8015: cmdAcc.frames and cmdAcc.comms must stay
// strictly bounded under thousands of unique synthetic frames and comms.
func TestStoreCommandFramesAndCommsBounded(t *testing.T) {
	t.Run("rejected stacks do not intern frames and comms stay capped", func(t *testing.T) {
		s := NewStore(20)
		t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)
		// Send 3000 unique stacks (each with a unique frame) and 500 unique comms
		// all inside a single 5-second bucket.
		for i := range 3000 {
			s.Record(Sample{
				CommandID: "jit0000001",
				Timestamp: t0.Add(time.Duration(i%1000) * time.Millisecond),
				Comm:      fmt.Sprintf("worker-%d", i%500),
				Frames: []ResolvedFrame{
					{Name: "jit.entry", Module: "v8", FrameType: "v8"},
					{Name: fmt.Sprintf("jit.anon_%d", i), Module: "v8", FrameType: "v8"},
				},
			})
		}

		s.mu.RLock()
		acc := s.cmds["jit0000001"]
		numComms := len(acc.comms)
		numFrames := len(acc.frames.Frames())
		numStacks := len(acc.buckets[t0.UnixMilli()].stacks)
		s.mu.RUnlock()

		if numComms > defaultMaxCmdComms {
			t.Fatalf("cmdAcc.comms grew to %d, want <= %d", numComms, defaultMaxCmdComms)
		}
		if numStacks > maxStacksPerBucket {
			t.Fatalf("bucket stacks grew to %d, want <= %d", numStacks, maxStacksPerBucket)
		}
		// Only the accepted stacks (maxStacksPerBucket) should have interned frames
		// (1 shared root + maxStacksPerBucket unique leaves).
		if wantMax := maxStacksPerBucket + 1; numFrames > wantMax {
			t.Fatalf("cmdAcc.frames grew to %d (rejected stacks leaked frames), want <= %d", numFrames, wantMax)
		}
	})

	t.Run("multi-bucket unique frames stay strictly within maxCmdFrames", func(t *testing.T) {
		s := NewStore(20)
		s.maxCmdFrames = 2048
		s.maxCmdComms = 64
		t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)

		// Record 100 unique frames per bucket across 50 buckets (5000 unique frames total),
		// plus re-record an already-known stack after hitting the cap.
		for b := range 50 {
			ts := t0.Add(time.Duration(b) * bucketDurationMs * time.Millisecond)
			for i := range 100 {
				s.Record(Sample{
					CommandID: "jit0000002",
					Timestamp: ts,
					Comm:      fmt.Sprintf("proc-%d", b*10+i%10),
					Frames: []ResolvedFrame{
						{Name: "root", FrameType: "v8"},
						{Name: fmt.Sprintf("fn_%d_%d", b, i), FrameType: "v8"},
					},
				})
			}
		}
		// Re-record a stack whose frames are already interned: must still be counted!
		s.Record(Sample{
			CommandID: "jit0000002",
			Timestamp: t0,
			Comm:      "proc-0",
			Frames: []ResolvedFrame{
				{Name: "root", FrameType: "v8"},
				{Name: "fn_0_0", FrameType: "v8"},
			},
		})

		s.mu.RLock()
		acc := s.cmds["jit0000002"]
		numFrames := len(acc.frames.Frames())
		numComms := len(acc.comms)
		s.mu.RUnlock()

		if numFrames > s.maxCmdFrames {
			t.Fatalf("cmdAcc.frames = %d, exceeds cap %d", numFrames, s.maxCmdFrames)
		}
		if numComms > s.maxCmdComms {
			t.Fatalf("cmdAcc.comms = %d, exceeds cap %d", numComms, s.maxCmdComms)
		}
		prof := s.Query("jit0000002", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "jit0000002"}}, true, &pb.ProfilerStatus{Active: true})
		if err := Admit(prof); err != nil {
			t.Fatalf("Admit failed on bounded profile: %v", err)
		}
		if prof.TotalSamples != 5001 {
			t.Fatalf("TotalSamples = %d, want 5001", prof.TotalSamples)
		}
		// Bucket 0 stack fn_0_0 should have count 2 (1 initial + 1 post-cap).
		foundReused := false
		for _, st := range prof.Stacks {
			if flame.Fold(prof.Frames, st) == "root;fn_0_0" && st.Samples == 2 {
				foundReused = true
				break
			}
		}
		if !foundReused {
			t.Fatalf("expected known stack root;fn_0_0 to keep incrementing after hitting frame cap")
		}
	})
}

// Regression test for board 7896: Symbolizer.Lookup must bound zero-sized ELF
// symbols using the next symbol's address and maxZeroSizeSymSlack so
// out-of-range addresses are never misattributed.
func TestSymbolizerLookupZeroSizeBounds(t *testing.T) {
	sym := NewSymbolizer(8)
	sym.entries["libtest"] = &binarySyms{
		syms: []elfSym{
			{addr: 0x1000, end: 0x1100, name: "sized_fn"},
			{addr: 0x2000, end: 0, name: "asm_close_next"},    // zero-sized, next sym 0x200 away
			{addr: 0x2200, end: 0, name: "asm_distant_next"},  // zero-sized, next sym 1 MiB away
			{addr: 0x102200, end: 0, name: "asm_last_symbol"}, // zero-sized trailing symbol
		},
	}

	cases := []struct {
		addr     uint64
		wantName string
		wantOK   bool
	}{
		{0x0fff, "", false},                            // before first symbol
		{0x1000, "sized_fn", true},                     // sized start
		{0x10ff, "sized_fn", true},                     // sized last byte
		{0x1100, "", false},                            // past sized symbol end
		{0x2000, "asm_close_next", true},               // zero-sized start
		{0x21ff, "asm_close_next", true},               // just before close next symbol
		{0x2200, "asm_distant_next", true},             // next symbol start
		{0x2200 + maxZeroSizeSymSlack - 1, "asm_distant_next", true},
		{0x2200 + maxZeroSizeSymSlack, "", false},      // past 64 KiB slack before distant next
		{0x080000, "", false},                          // far in gap before distant next
		{0x102200, "asm_last_symbol", true},            // trailing zero-sized start
		{0x102200 + maxZeroSizeSymSlack - 1, "asm_last_symbol", true},
		{0x102200 + maxZeroSizeSymSlack, "", false},    // past trailing symbol slack
		{0x7fff00001234, "", false},                    // arbitrarily high address
	}
	for _, tc := range cases {
		gotName, _, _, _, gotOK := sym.Lookup("libtest", tc.addr)
		if gotOK != tc.wantOK || gotName != tc.wantName {
			t.Errorf("Lookup(%#x) = (%q, %v), want (%q, %v)", tc.addr, gotName, gotOK, tc.wantName, tc.wantOK)
		}
	}
}

// Regression test for board 8293 (Finding #83): QueryProfile on a persisted
// Snapshot must filter Stacks, TopFunctions, and MatchedSamples to the
// requested time window using per-bucket stack counts, never leaking stacks
// from other buckets.
func TestPersistedSnapshotQueryProfileTimeWindowFiltering(t *testing.T) {
	s := NewStore(20)
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)

	// Bucket 0 [t0, t0+5s): 4 samples of stack1 + 2 samples of sharedStack.
	for i := range 4 {
		s.Record(Sample{
			CommandID: "persisted01",
			Timestamp: t0.Add(time.Duration(i*100) * time.Millisecond),
			Comm:      "phase1",
			Frames: []ResolvedFrame{
				{Name: "main", FrameType: "go"},
				{Name: "phase1.work", FrameType: "go"},
			},
		})
	}
	for i := range 2 {
		s.Record(Sample{
			CommandID: "persisted01",
			Timestamp: t0.Add(time.Duration((4+i)*100) * time.Millisecond),
			Comm:      "common",
			Frames: []ResolvedFrame{
				{Name: "main", FrameType: "go"},
				{Name: "shared.helper", FrameType: "go"},
			},
		})
	}

	// Bucket 1 [t0+5s, t0+10s): 5 samples of stack2 + 3 samples of sharedStack.
	b1Start := t0.Add(5 * time.Second)
	for i := range 5 {
		s.Record(Sample{
			CommandID: "persisted01",
			Timestamp: b1Start.Add(time.Duration(i*100) * time.Millisecond),
			Comm:      "phase2",
			Frames: []ResolvedFrame{
				{Name: "main", FrameType: "go"},
				{Name: "ext4_sync", FrameType: "kernel"},
			},
		})
	}
	for i := range 3 {
		s.Record(Sample{
			CommandID: "persisted01",
			Timestamp: b1Start.Add(time.Duration((5+i)*100) * time.Millisecond),
			Comm:      "common",
			Frames: []ResolvedFrame{
				{Name: "main", FrameType: "go"},
				{Name: "shared.helper", FrameType: "go"},
			},
		})
	}

	status := &pb.ProfilerStatus{Active: true, Engine: EngineName, SampleHz: 20}
	snap := s.Snapshot("persisted01", status)

	path := filepath.Join(t.TempDir(), "persisted01.pb.gz")
	if err := SaveProfile(path, snap); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	loaded, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}

	// Select only Bucket 0 [t0, t0+5s).
	win0 := QueryProfile(loaded, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "persisted01"},
		FromMs: t0.UnixMilli(),
		ToMs:   t0.UnixMilli() + bucketDurationMs - 1,
	})
	if err := Admit(win0); err != nil {
		t.Fatalf("Admit(win0): %v", err)
	}
	if win0.MatchedSamples != 6 || win0.UserSamples != 6 || win0.KernelSamples != 0 {
		t.Fatalf("win0 counts: matched=%d user=%d kernel=%d, want 6/6/0", win0.MatchedSamples, win0.UserSamples, win0.KernelSamples)
	}
	if got, want := folded(win0), []string{"main;phase1.work 4", "main;shared.helper 2"}; !slices.Equal(got, want) {
		t.Fatalf("win0 stacks = %q, want %q", got, want)
	}

	// Select only Bucket 1 [t0+5s, t0+10s).
	win1 := QueryProfile(loaded, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "persisted01"},
		FromMs: b1Start.UnixMilli(),
		ToMs:   b1Start.UnixMilli() + bucketDurationMs - 1,
	})
	if err := Admit(win1); err != nil {
		t.Fatalf("Admit(win1): %v", err)
	}
	if win1.MatchedSamples != 8 || win1.UserSamples != 3 || win1.KernelSamples != 5 {
		t.Fatalf("win1 counts: matched=%d user=%d kernel=%d, want 8/3/5", win1.MatchedSamples, win1.UserSamples, win1.KernelSamples)
	}
	if got, want := folded(win1), []string{"main;ext4_sync_[k] 5", "main;shared.helper 3"}; !slices.Equal(got, want) {
		t.Fatalf("win1 stacks = %q, want %q", got, want)
	}
	var top1 []string
	for _, fn := range win1.TopFunctions {
		top1 = append(top1, fmt.Sprintf("%s %d/%d", win1.Frames[fn.Frame].Name, fn.SelfSamples, fn.TotalSamples))
	}
	if want := []string{"ext4_sync 5/5", "shared.helper 3/3", "main 0/8"}; !slices.Equal(top1, want) {
		t.Fatalf("win1 top functions = %q, want %q", top1, want)
	}

	// Querying a window must not mutate the persisted snapshot's lifetime counts.
	full := QueryProfile(loaded, &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "persisted01"}})
	if full.MatchedSamples != 14 {
		t.Fatalf("loaded profile was mutated: full matched=%d, want 14", full.MatchedSamples)
	}
	if got, want := folded(full), []string{"main;shared.helper 5", "main;ext4_sync_[k] 5", "main;phase1.work 4"}; !slices.Equal(got, want) {
		t.Fatalf("full stacks = %q, want %q", got, want)
	}

	// Empty window on persisted profile returns 0 stacks and 0 matched samples.
	empty := QueryProfile(loaded, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "persisted01"},
		FromMs: t0.Add(60 * time.Second).UnixMilli(),
		ToMs:   t0.Add(70 * time.Second).UnixMilli(),
	})
	if empty.MatchedSamples != 0 || len(empty.Stacks) != 0 || len(empty.Buckets) != 0 {
		t.Fatalf("empty persisted window = matched:%d stacks:%d buckets:%d, want 0/0/0", empty.MatchedSamples, len(empty.Stacks), len(empty.Buckets))
	}
}

// Regression test for board 8289 (Finding #84): distinct stacks across a
// command's entire lifetime (across many buckets) stay strictly capped at
// maxCmdStacks while sample totals, kernel/user splits, and known stacks keep
// counting accurately.
func TestStoreCommandStacksCappedAcrossBuckets(t *testing.T) {
	s := NewStore(20)
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)

	// Record 250 unique stacks per bucket across 25 buckets (6250 unique stacks
	// total > maxStacksPerCmd=4000).
	const (
		numBuckets       = 25
		stacksPerBucket  = 250
		expectedRecorded = numBuckets * stacksPerBucket
	)
	for b := range numBuckets {
		ts := t0.Add(time.Duration(b) * bucketDurationMs * time.Millisecond)
		for i := range stacksPerBucket {
			s.Record(Sample{
				CommandID: "cap0000001",
				Timestamp: ts,
				Comm:      "worker",
				Frames: []ResolvedFrame{
					{Name: "root", FrameType: "go"},
					{Name: fmt.Sprintf("leaf_%d_%d", b, i), FrameType: "go"},
				},
			})
		}
	}
	// Re-sample an already-interned stack in the last bucket after hitting the cap:
	// it must still be tracked in that bucket's stacks map.
	lastTS := t0.Add(time.Duration(numBuckets-1) * bucketDurationMs * time.Millisecond)
	s.Record(Sample{
		CommandID: "cap0000001",
		Timestamp: lastTS,
		Comm:      "worker",
		Frames: []ResolvedFrame{
			{Name: "root", FrameType: "go"},
			{Name: "leaf_0_0", FrameType: "go"},
		},
	})

	s.mu.RLock()
	acc := s.cmds["cap0000001"]
	interned := len(acc.stackList)
	indexed := len(acc.stackIndex)
	total := acc.total
	lastBucket := acc.buckets[lastTS.UnixMilli()]
	lastBucketSamples := lastBucket.samples
	lastBucketUser := lastBucket.user
	s.mu.RUnlock()

	if interned != maxStacksPerCmd || indexed != maxStacksPerCmd {
		t.Fatalf("interned stacks = %d (index %d), want exactly cap %d", interned, indexed, maxStacksPerCmd)
	}
	if total != expectedRecorded+1 {
		t.Fatalf("acc.total = %d, want %d", total, expectedRecorded+1)
	}
	if lastBucketSamples != stacksPerBucket+1 || lastBucketUser != stacksPerBucket+1 {
		t.Fatalf("last bucket samples/user = %d/%d, want %d", lastBucketSamples, lastBucketUser, stacksPerBucket+1)
	}

	// Querying the last bucket must still return the reused stack (leaf_0_0)
	// and accurate bucket sample count.
	win := s.Query("cap0000001", &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "cap0000001"},
		FromMs: lastTS.UnixMilli(),
		ToMs:   lastTS.UnixMilli() + bucketDurationMs - 1,
	}, true, &pb.ProfilerStatus{Active: true})
	if err := Admit(win); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if win.MatchedSamples != stacksPerBucket+1 {
		t.Fatalf("last bucket matched = %d, want %d", win.MatchedSamples, stacksPerBucket+1)
	}
	if got, want := folded(win), []string{"root;leaf_0_0 1"}; !slices.Equal(got, want) {
		t.Fatalf("last bucket stacks = %q, want %q", got, want)
	}
}

// Regression test for board 8293 (Finding #85): when a command's frame table
// is full and a later bucket receives only unseen leaf frames, that bucket
// must report noTopFrame instead of unrelated frame index 0.
func TestSaturatedBucketDoesNotReportUnrelatedTopFrame(t *testing.T) {
	s := NewStore(20)
	s.maxCmdFrames = 1
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)

	// Bucket 0 fills the 1-frame table with "old".
	s.Record(Sample{
		CommandID: "sat0000001",
		Timestamp: t0,
		Comm:      "app",
		Frames:    []ResolvedFrame{{Name: "old", FrameType: "go"}},
	})
	// Bucket 1 only sees "new", which cannot be interned.
	b1TS := t0.Add(bucketDurationMs * time.Millisecond)
	s.Record(Sample{
		CommandID: "sat0000001",
		Timestamp: b1TS,
		Comm:      "app",
		Frames:    []ResolvedFrame{{Name: "new", FrameType: "go"}},
	})

	status := &pb.ProfilerStatus{Active: true}
	prof := s.Query("sat0000001", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "sat0000001"}}, true, status)
	if len(prof.Buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(prof.Buckets))
	}
	if prof.Buckets[0].TopFrame >= uint32(len(prof.Frames)) || prof.Frames[prof.Buckets[0].TopFrame].Name != "old" {
		t.Fatalf("bucket 0 top frame = %d (%v), want old", prof.Buckets[0].TopFrame, prof.Frames)
	}
	if prof.Buckets[1].TopFrame != noTopFrame {
		t.Fatalf("saturated bucket 1 top frame = %d, want noTopFrame (%d)", prof.Buckets[1].TopFrame, noTopFrame)
	}

	// Snapshot + persist + LoadProfile/Admit must accept noTopFrame cleanly,
	// and querying bucket 1 on the persisted snapshot must keep noTopFrame and
	// not leak bucket 0's "old" stack.
	snap := s.Snapshot("sat0000001", status)
	path := filepath.Join(t.TempDir(), "sat0000001.pb.gz")
	if err := SaveProfile(path, snap); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	loaded, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	win1 := QueryProfile(loaded, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_CommandId{CommandId: "sat0000001"},
		FromMs: b1TS.UnixMilli(),
		ToMs:   b1TS.UnixMilli() + bucketDurationMs - 1,
	})
	if len(win1.Buckets) != 1 || win1.Buckets[0].TopFrame != noTopFrame {
		t.Fatalf("persisted bucket 1 = %+v, want TopFrame=%d", win1.Buckets, noTopFrame)
	}
	if len(win1.Stacks) != 0 {
		t.Fatalf("persisted saturated bucket 1 leaked stacks: %q", folded(win1))
	}
}

func TestMergeProfiles(t *testing.T) {
	t0 := time.UnixMilli(1_795_300_000_000 / bucketDurationMs * bucketDurationMs)
	status := &pb.ProfilerStatus{Active: true, Engine: EngineName, SampleHz: 20}

	hostStore := NewStore(20)
	for range 3 {
		hostStore.Record(Sample{
			Timestamp: t0.Add(100 * time.Millisecond),
			Comm:      "firecracker",
			Frames: []ResolvedFrame{
				{Name: "vcpu_run", FrameType: "native"},
				{Name: "kvm_arch_vcpu_ioctl_run", FrameType: "kernel"},
			},
		})
	}
	vmStore := NewStore(20)
	for range 4 {
		vmStore.Record(Sample{
			CommandID: "cmd1",
			Timestamp: t0.Add(200 * time.Millisecond),
			Comm:      "python3",
			Frames: []ResolvedFrame{
				{Name: "_PyEval_EvalFrameDefault", FrameType: "native"},
				{Name: "hot_loop", FrameType: "cpython"},
			},
		})
	}

	hostProf := hostStore.QueryNode("ubu1", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "ubu1"}, Limit: 200}, status)
	vmProf := vmStore.QueryNode("vm:ws1", &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "vm:ws1"}, Limit: 200}, status)

	merged := MergeProfiles("ubu1", hostProf, []*pb.CommandProfile{vmProf}, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "ubu1"},
		Limit:  200,
	})
	if err := Admit(merged); err != nil {
		t.Fatalf("Admit(merged): %v", err)
	}
	if !merged.Active || merged.TotalSamples != 7 || merged.MatchedSamples != 7 || merged.KernelSamples != 3 || merged.UserSamples != 4 {
		t.Fatalf("merged totals = active:%v total:%d matched:%d k:%d u:%d, want true/7/7/3/4",
			merged.Active, merged.TotalSamples, merged.MatchedSamples, merged.KernelSamples, merged.UserSamples)
	}
	wantStacks := []string{
		"vm:ws1;python3;_PyEval_EvalFrameDefault;hot_loop 4",
		"firecracker;vcpu_run;kvm_arch_vcpu_ioctl_run_[k] 3",
	}
	if got := folded(merged); !slices.Equal(got, wantStacks) {
		t.Fatalf("merged stacks = %q, want %q", got, wantStacks)
	}
	if len(merged.Buckets) != 1 || merged.Buckets[0].Samples != 7 {
		t.Fatalf("merged buckets = %+v, want 1 bucket with 7 samples", merged.Buckets)
	}

	filtered := MergeProfiles("ubu1", hostProf, []*pb.CommandProfile{vmProf}, &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "ubu1"},
		Query:  "hot_loop",
		Limit:  200,
	})
	if filtered.MatchedSamples != 4 || len(filtered.Stacks) != 1 {
		t.Fatalf("filtered merged profile = matched:%d stacks:%d, want 4/1", filtered.MatchedSamples, len(filtered.Stacks))
	}
}
