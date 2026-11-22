package profiler

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestExtractCommandID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0::/hotbed.slice/hotbed-2plyu4zxok.service\n", "2plyu4zxok"},
		{"0::/user.slice/user-hotbed.slice/hotbed-abc1234567.service\n", "abc1234567"},
		{"0::/hotbed_bench.slice/hotbed-xyz9876543.service/sub\n", "xyz9876543"},
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
	all := s.Query("cmd1234567", &pb.GetCommandProfileRequest{Id: "cmd1234567"}, true, status)
	if all.TotalSamples != 10 || all.MatchedSamples != 10 || all.UserSamples != 6 || all.KernelSamples != 4 {
		t.Fatalf("unexpected counts: total=%d matched=%d user=%d kernel=%d", all.TotalSamples, all.MatchedSamples, all.UserSamples, all.KernelSamples)
	}
	if len(all.Buckets) != 3 {
		t.Fatalf("expected 3 time buckets, got %d", len(all.Buckets))
	}
	// Verify recursive ssa.walk wasn't double-counted in total_samples (should be 6, not 12).
	for _, fn := range all.TopFunctions {
		if fn.Name == "ssa.walk" && fn.TotalSamples != 6 {
			t.Fatalf("recursive frame ssa.walk total_samples = %d, want 6", fn.TotalSamples)
		}
	}

	// Filter by query + frameType + comm.
	filtered := s.Query("cmd1234567", &pb.GetCommandProfileRequest{Id: "cmd1234567", Query: "vfs_write", FrameType: "kernel", Comm: "worker.test"}, true, status)
	if filtered.MatchedSamples != 4 || len(filtered.Stacks) != 1 || filtered.Stacks[0].LeafType != "kernel" {
		t.Fatalf("unexpected filtered profile: %+v", filtered)
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
	reFiltered := QueryProfile(loaded, &pb.GetCommandProfileRequest{Id: "cmd1234567", Query: "regalloc"})
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
	sym.IndexELF("self-id", exe, f)
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
