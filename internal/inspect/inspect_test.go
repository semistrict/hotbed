package inspect

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestParseProcStat(t *testing.T) {
	// Comm contains spaces and parentheses; parser must find the LAST ')'.
	// Field #25 is rsslim, #26..#37 are 12 zero fields, #38 is exit_signal (17), #39 is processor (5).
	line := "12345 (my (weird) proc) S 100 12345 100 0 -1 4194304 10 0 2 0 250 75 0 0 20 0 4 0 9999 104857600 4096 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 5"
	got, err := ParseProcStat(line)
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	want := ProcStat{
		PID:       12345,
		Comm:      "my (weird) proc",
		State:     "S",
		PPID:      100,
		MinFlt:    10,
		MajFlt:    2,
		UTime:     250,
		STime:     75,
		Threads:   4,
		VSize:     104857600,
		RSS:       4096,
		Processor: 5,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

func TestParseProcNet(t *testing.T) {
	m := map[uint64]SocketInfo{}
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 912044 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 0100007F:D428 01 00000000:00000000 00:00000000 00000000  1000        0 912089 1 0000000000000000 20 4 30 10 -1`
	ParseProcNetInet("tcp", tcp, m)
	if got := m[912044]; got != (SocketInfo{Proto: "tcp", State: "LISTEN", Local: "127.0.0.1:8080", Remote: "0.0.0.0:0"}) {
		t.Fatalf("unexpected listen socket: %+v", got)
	}
	if got := m[912089]; got != (SocketInfo{Proto: "tcp", State: "ESTABLISHED", Local: "127.0.0.1:8080", Remote: "127.0.0.1:54312"}) {
		t.Fatalf("unexpected established socket: %+v", got)
	}

	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1BD0 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 55511 1`
	ParseProcNetInet("tcp6", tcp6, m)
	if got := m[55511]; got.Local != "[::1]:7120" || got.State != "LISTEN" {
		t.Fatalf("unexpected tcp6 socket: %+v", got)
	}

	unixData := `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 77711 /run/hotbed/app.sock`
	ParseProcNetUnix(unixData, m)
	if got := m[77711]; got != (SocketInfo{Proto: "unix", State: "LISTEN", Local: "/run/hotbed/app.sock"}) {
		t.Fatalf("unexpected unix socket: %+v", got)
	}
}

func TestClassifyFDAndFDInfo(t *testing.T) {
	if k, ino := ClassifyFDTarget("socket:[912044]"); k != "socket" || ino != 912044 {
		t.Fatalf("got (%q, %d)", k, ino)
	}
	if k, ino := ClassifyFDTarget("pipe:[884120]"); k != "pipe" || ino != 884120 {
		t.Fatalf("got (%q, %d)", k, ino)
	}
	if k, _ := ClassifyFDTarget("anon_inode:[eventpoll]"); k != "anon_inode" {
		t.Fatalf("got %q", k)
	}
	if k, _ := ClassifyFDTarget("/tmp/foo (deleted)"); k != "deleted" {
		t.Fatalf("got %q", k)
	}
	if k, _ := ClassifyFDTarget("/work/hotbed/main.go"); k != "file" {
		t.Fatalf("got %q", k)
	}
	pos, mode, _, _ := ParseFDInfoDetailed("pos:\t4096\nflags:\t02000002\nmnt_id:\t42\n")
	if pos != 4096 || mode != "rw" {
		t.Fatalf("got pos=%d mode=%q", pos, mode)
	}
}

func TestParseCgroupAndStacksAndLimits(t *testing.T) {
	some, full, someTot, fullTot := parsePSIAll("some avg10=1.25 avg60=0.50 avg300=0.10 total=1234\nfull avg10=0.40 avg60=0.10 avg300=0.01 total=50\n")
	if some != 1.25 || full != 0.40 || someTot != 1234 || fullTot != 50 {
		t.Fatalf("unexpected PSI: some=%v full=%v someTot=%d fullTot=%d", some, full, someTot, fullTot)
	}
	ioRows := ParseCgroupIO("259:0 rbytes=1024 wbytes=2048 rios=3 wios=4 dbytes=0 dios=0\n")
	if len(ioRows) != 1 || ioRows[0].Rbytes != 1024 || ioRows[0].Wios != 4 {
		t.Fatalf("unexpected cgroup IO: %+v", ioRows)
	}
	frames := ParseKernelStack("[<0>] futex_wait_queue+0x60/0xa0\n[<0>] do_syscall_64+0x5c/0x90\n")
	if diff := cmp.Diff([]string{"futex_wait_queue+0x60/0xa0", "do_syscall_64+0x5c/0x90"}, frames); diff != "" {
		t.Fatalf("kstack mismatch:\n%s", diff)
	}
	lims := ParseProcLimits("Limit                     Soft Limit           Hard Limit           Units\nMax open files            1024                 524288               files\nMax stack size            8388608              unlimited            bytes\n")
	if len(lims) != 2 || lims[0].Name != "Max open files" || lims[0].Hard != "524288" || lims[1].Hard != "unlimited" {
		t.Fatalf("unexpected limits: %+v", lims)
	}
}

func TestParseStraceAndPerf(t *testing.T) {
	straceOut := `% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 60.00    0.300000          30     10000        12 futex
 40.00    0.200000          20     10000           read
------ ----------- ----------- --------- --------- ----------------
100.00    0.500000          25     20000        12 total
`
	sc := ParseStraceSummary(straceOut)
	if len(sc) != 2 || sc[0].Name != "futex" || sc[0].Errors != 12 || sc[1].Name != "read" || sc[1].Errors != 0 {
		t.Fatalf("unexpected strace summary: %+v", sc)
	}

	perfStatOut := ` Performance counter stats for process id '1234':

         1,234.56 msec task-clock                #    1.234 CPUs utilized
            1,000      context-switches          #    0.810 K/sec
    4,000,000,000      cycles                    #    3.240 GHz
    8,000,000,000      instructions              #    2.00  insn per cycle
       1.000500123 seconds time elapsed
`
	counters := ParsePerfStat(perfStatOut)
	if len(counters) != 5 || counters[0].Name != "task-clock" || counters[0].Unit != "msec" || counters[3].Name != "instructions" || counters[4].Name != "time-elapsed" {
		t.Fatalf("unexpected perf stat: %+v", counters)
	}

	perfScriptOut := `compile 41289 100.001:   10000000 cpu-clock:
	    7a4b20 ssa.regalloc+0x10 (/usr/local/go/pkg/tool/compile)
	    791044 ssa.Compile+0x40 (/usr/local/go/pkg/tool/compile)

compile 41289 100.011:   10000000 cpu-clock:
	ffffffff810a1234 zfs_write+0x14 ([kernel.kallsyms])
	    411000 syscall.Syscall+0x10 (/usr/local/go/pkg/tool/compile)
`
	frames, stacks, fns := ParsePerfScript(perfScriptOut, 20)
	var folded []string
	for _, st := range stacks {
		folded = append(folded, flame.Fold(frames, st))
	}
	if want := []string{"compile;ssa.Compile;ssa.regalloc", "compile;syscall.Syscall;zfs_write_[k]"}; !slices.Equal(folded, want) {
		t.Fatalf("folded stacks = %q, want %q", folded, want)
	}
	var top []string
	for _, fn := range fns {
		top = append(top, fmt.Sprintf("%s %s %d/%d", frames[fn.Frame].Name, frames[fn.Frame].FrameType, fn.SelfSamples, fn.TotalSamples))
	}
	// The process at a stack's root is not a function.
	if want := []string{"ssa.regalloc native 1/1", "zfs_write kernel 1/1", "ssa.Compile native 0/1", "syscall.Syscall native 0/1"}; !slices.Equal(top, want) {
		t.Fatalf("top functions = %q, want %q", top, want)
	}
}

func TestParsePerfScriptKernelFrames(t *testing.T) {
	perfScriptOut := `worker 5512 200.001:   10000000 cpu-clock:
	ffffffffc0102000 ext4_file_write_iter+0x24 ([ext4])
	ffffffff810a1234 vfs_write_[k]+0x10 (/usr/lib/debug/boot/vmlinux-6.12)
	ffffffff81001000 do_syscall_64+0x5c ([kernel.kallsyms])
	ffffffff81000800 entry_SYSCALL_64+0x10 (kernel)
	    7f0000001000 __vdso_clock_gettime+0x10 ([vdso])
	    411000 syscall.Syscall+0x10 (/usr/local/go/pkg/tool/compile)
`
	frames, stacks, _ := ParsePerfScript(perfScriptOut, 20)
	if len(stacks) != 1 {
		t.Fatalf("expected 1 stack, got %d", len(stacks))
	}
	wantFolded := "worker;syscall.Syscall;__vdso_clock_gettime;entry_SYSCALL_64_[k];do_syscall_64_[k];vfs_write_[k];ext4_file_write_iter_[k]"
	if got := flame.Fold(frames, stacks[0]); got != wantFolded {
		t.Fatalf("folded stack =\n  %s\nwant:\n  %s", got, wantFolded)
	}
	// Verify every intermediate and leaf kernel frame has FrameType=="kernel"
	// while [vdso] and native frames stay "native".
	typeByName := map[string]string{}
	for _, fr := range frames {
		typeByName[fr.Name] = fr.FrameType
	}
	for _, kname := range []string{"entry_SYSCALL_64", "do_syscall_64", "vfs_write", "ext4_file_write_iter"} {
		if typeByName[kname] != "kernel" {
			t.Errorf("frame %q has FrameType=%q, want kernel", kname, typeByName[kname])
		}
	}
	for _, uname := range []string{"syscall.Syscall", "__vdso_clock_gettime"} {
		if typeByName[uname] != "native" {
			t.Errorf("frame %q has FrameType=%q, want native", uname, typeByName[uname])
		}
	}
}

func TestBuildBPFTraceProgramPIDFilter(t *testing.T) {
	cg := "/sys/fs/cgroup/hotbed.slice/hotbed-abc123.service"
	kinds := []pb.TraceKind{
		pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
		pb.TraceKind_TRACE_KIND_BPF_OFFCPU,
		pb.TraceKind_TRACE_KIND_BPF_NET,
	}
	for _, kind := range kinds {
		_, progNoPID, err := buildBPFTraceProgram(kind, cg, 5, 0)
		if err != nil {
			t.Fatalf("buildBPFTraceProgram(%v, pid=0): %v", kind, err)
		}
		if !strings.Contains(progNoPID, `cgroup == cgroupid("`+cg+`")`) {
			t.Errorf("%v without pid missing cgroup filter: %s", kind, progNoPID)
		}
		if strings.Contains(progNoPID, "pid == ") {
			t.Errorf("%v without pid unexpectedly has pid filter: %s", kind, progNoPID)
		}

		_, progPID, err := buildBPFTraceProgram(kind, cg, 5, 4242)
		if err != nil {
			t.Fatalf("buildBPFTraceProgram(%v, pid=4242): %v", kind, err)
		}
		if kind == pb.TraceKind_TRACE_KIND_BPF_OFFCPU {
			if !strings.Contains(progPID, `cgroup == cgroupid("`+cg+`") && (pid == 4242 || args.prev_pid == 4242)`) {
				t.Errorf("offcpu with pid=4242 missing PID/prev_pid predicate: %s", progPID)
			}
		} else {
			if !strings.Contains(progPID, `cgroup == cgroupid("`+cg+`") && pid == 4242`) {
				t.Errorf("%v with pid=4242 missing PID predicate: %s", kind, progPID)
			}
		}
	}

	if _, _, err := buildBPFTraceProgram(pb.TraceKind_TRACE_KIND_STRACE, cg, 5, 0); err == nil {
		t.Fatalf("expected error for non-BPF trace kind")
	}
}

func TestBuildBPFTraceProgramForCgroups(t *testing.T) {
	cgs := []string{
		"/sys/fs/cgroup/hotbed.slice/hotbed-abc123.service/command",
		"/sys/fs/cgroup/hotbed.slice/hotbed-abc123.service/command/payload",
	}
	wantGroup := `(cgroup == cgroupid("` + cgs[0] + `") || cgroup == cgroupid("` + cgs[1] + `"))`
	kinds := []pb.TraceKind{
		pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP,
		pb.TraceKind_TRACE_KIND_BPF_VFSSTAT,
		pb.TraceKind_TRACE_KIND_BPF_OFFCPU,
		pb.TraceKind_TRACE_KIND_BPF_NET,
	}
	for _, kind := range kinds {
		_, prog, err := buildBPFTraceProgramForCgroups(kind, cgs, 3, 0)
		if err != nil {
			t.Fatalf("buildBPFTraceProgramForCgroups(%v, pid=0): %v", kind, err)
		}
		if !strings.Contains(prog, "/"+wantGroup+"/") {
			t.Errorf("%v missing parenthesized multi-cgroup predicate: %s", kind, prog)
		}
		if !strings.Contains(prog, "interval:s:3 { exit(); }") {
			t.Errorf("%v missing interval exit: %s", kind, prog)
		}

		_, progPID, err := buildBPFTraceProgramForCgroups(kind, cgs, 0, 777)
		if err != nil {
			t.Fatalf("buildBPFTraceProgramForCgroups(%v, pid=777): %v", kind, err)
		}
		if !strings.Contains(progPID, "interval:s:1 { exit(); }") {
			t.Errorf("%v did not clamp durSec=0 to 1: %s", kind, progPID)
		}
		if kind == pb.TraceKind_TRACE_KIND_BPF_OFFCPU {
			if !strings.Contains(progPID, wantGroup+` && (pid == 777 || args.prev_pid == 777)`) {
				t.Errorf("offcpu multi-cgroup + pid predicate mismatch: %s", progPID)
			}
		} else {
			if !strings.Contains(progPID, wantGroup+` && pid == 777`) {
				t.Errorf("%v multi-cgroup + pid predicate mismatch: %s", kind, progPID)
			}
		}
	}

	if _, _, err := buildBPFTraceProgramForCgroups(pb.TraceKind_TRACE_KIND_BPF_VFSSTAT, nil, 2, 0); err == nil {
		t.Fatal("expected error for empty cgroup list")
	}
	if _, _, err := buildBPFTraceProgramForCgroups(pb.TraceKind_TRACE_KIND_BPF_VFSSTAT, []string{cgs[0], `/sys/fs/cgroup/bad"); id`}, 2, 0); err == nil {
		t.Fatal("expected error when any cgroup path in list is unsafe")
	}
}

func TestValidateCgroupPathAndBPFTraceInjection(t *testing.T) {
	valid := []string{
		"/sys/fs/cgroup/system.slice/hotbed-cmd_abc.service",
		"/sys/fs/cgroup/hotbed.slice/hotbed-abc123.service",
		"/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app:1.scope",
	}
	for _, cg := range valid {
		if err := validateCgroupPath(cg); err != nil {
			t.Errorf("validateCgroupPath(%q) unexpectedly failed: %v", cg, err)
		}
		_, prog, err := buildBPFTraceProgram(pb.TraceKind_TRACE_KIND_BPF_VFSSTAT, cg, 5, 0)
		if err != nil {
			t.Errorf("buildBPFTraceProgram(%q) unexpectedly failed: %v", cg, err)
		}
		if !strings.Contains(prog, fmt.Sprintf(`cgroup == cgroupid(%q)`, cg)) {
			t.Errorf("program missing quoted cgroupid for %q: %s", cg, prog)
		}
	}

	invalid := []string{
		"",
		"/",
		"/sys/fs/cgroup",
		"/sys/fs/cgroup/",
		"/etc/passwd",
		"system.slice/hotbed-cmd.service",
		"/sys/fs/cgroup/..",
		"/sys/fs/cgroup/system.slice/../../etc/passwd",
		"/sys/fs/cgroup/./foo",
		"/sys/fs/cgroup/foo//bar",
		`/foo"); system("id"); //`,
		`/sys/fs/cgroup/foo"); system("id"); //`,
		"/sys/fs/cgroup/foo'bar",
		`/sys/fs/cgroup/foo\bar`,
		"/sys/fs/cgroup/foo\nbar",
		"/sys/fs/cgroup/foo\rbar",
		"/sys/fs/cgroup/foo\x00bar",
		"/sys/fs/cgroup/foo bar",
		"/sys/fs/cgroup/foo;id",
		"/sys/fs/cgroup/" + strings.Repeat("a", 512),
	}
	for _, cg := range invalid {
		if err := validateCgroupPath(cg); err == nil {
			t.Errorf("validateCgroupPath(%q) should have failed", cg)
		}
		if _, _, err := buildBPFTraceProgram(pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP, cg, 5, 0); err == nil {
			t.Errorf("buildBPFTraceProgram(%q) should have rejected unsafe cgroup path", cg)
		}
	}
}

func TestGDBBacktraceArgsAndEnvHardening(t *testing.T) {
	gotAll := gdbBacktraceArgs(4242, 0)
	wantAll := []string{
		"-nx",
		"-iex", "set auto-load off",
		"-batch",
		"-ex", "set pagination off",
		"-ex", "thread apply all bt",
		"-p", "4242",
	}
	if !slices.Equal(gotAll, wantAll) {
		t.Fatalf("gdbBacktraceArgs(4242, 0) = %q, want %q", gotAll, wantAll)
	}

	gotLimited := gdbBacktraceArgs(99, 48)
	wantLimited := []string{
		"-nx",
		"-iex", "set auto-load off",
		"-batch",
		"-ex", "set pagination off",
		"-ex", "thread apply all bt 48",
		"-p", "99",
	}
	if !slices.Equal(gotLimited, wantLimited) {
		t.Fatalf("gdbBacktraceArgs(99, 48) = %q, want %q", gotLimited, wantLimited)
	}

	env := sanitizedTracerEnv([]string{"PATH=/usr/bin", "HOME=/root", "XDG_CONFIG_HOME=/root/.config", "LANG=C"})
	wantEnv := []string{"PATH=/usr/bin", "LANG=C", "HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent"}
	if !slices.Equal(env, wantEnv) {
		t.Fatalf("sanitizedTracerEnv = %q, want %q", env, wantEnv)
	}
}

func TestParsePerfStatTrailingAdviceAndNotCounted(t *testing.T) {
	perfOut := `WARNING: A bogus warning line before stats

 Performance counter stats for process id '1234':

         1,234.56 msec task-clock                #    1.234 CPUs utilized
            1,000      context-switches          #    0.810 K/sec
    4,000,000,000      cycles                    #    3.240 GHz                       (83.33%)
    <not counted>      cache-misses                                                   (0.00%)
    <not counted>      cpu_core/branch-misses/   #    0.00% of all branches           (0.00%)
    <not supported>    stalled-cycles-frontend

       1.000500123 seconds time elapsed

       0.980123000 seconds user
       0.020456000 seconds sys


Some events weren't counted. Try disabling the NMI watchdog:
	echo 0 > /proc/sys/kernel/nmi_watchdog
	perf stat ...
	echo 1 > /proc/sys/kernel/nmi_watchdog
`
	got := ParsePerfStat(perfOut)
	type row struct {
		Name, Value, Unit, Note string
	}
	gotRows := make([]row, len(got))
	for i, c := range got {
		gotRows[i] = row{Name: c.Name, Value: c.Value, Unit: c.Unit, Note: c.Note}
	}
	wantRows := []row{
		{Name: "task-clock", Value: "1,234.56", Unit: "msec", Note: "1.234 CPUs utilized"},
		{Name: "context-switches", Value: "1,000", Unit: "", Note: "0.810 K/sec"},
		{Name: "cycles", Value: "4,000,000,000", Unit: "", Note: "3.240 GHz                       (83.33%)"},
		{Name: "cache-misses", Value: "<not counted>", Unit: "", Note: ""},
		{Name: "cpu_core/branch-misses/", Value: "<not counted>", Unit: "", Note: "0.00% of all branches           (0.00%)"},
		{Name: "stalled-cycles-frontend", Value: "<not supported>", Unit: "", Note: ""},
		{Name: "time-elapsed", Value: "1.000500123", Unit: "seconds", Note: ""},
	}
	if diff := cmp.Diff(wantRows, gotRows); diff != "" {
		t.Fatalf("ParsePerfStat mismatch (-want +got):\n%s", diff)
	}

	// Prose without a "seconds time elapsed" footer must also be ignored.
	proseOnly := `Some events weren't counted. Try disabling the NMI watchdog:
	echo 0 > /proc/sys/kernel/nmi_watchdog
	perf stat ...
	echo 1 > /proc/sys/kernel/nmi_watchdog
`
	if got := ParsePerfStat(proseOnly); len(got) != 0 {
		t.Fatalf("expected 0 counters from advisory prose, got %+v", got)
	}
}

func TestBuildBPFTraceProgramOffCPUAttribution(t *testing.T) {
	cg := "/sys/fs/cgroup/hotbed.slice/hotbed-abc123.service"
	_, prog, err := buildBPFTraceProgram(pb.TraceKind_TRACE_KIND_BPF_OFFCPU, cg, 5, 0)
	if err != nil {
		t.Fatalf("buildBPFTraceProgram(OFFCPU): %v", err)
	}

	// Split the two sched_switch probes so we assert switch-out vs switch-in separately.
	secondIdx := strings.Index(prog, "tracepoint:sched:sched_switch /@start[args.next_pid]/")
	if secondIdx < 0 {
		t.Fatalf("missing switch-in probe in offcpu program: %s", prog)
	}
	switchOut := prog[:secondIdx]
	rest := prog[secondIdx:]
	endIdx := strings.Index(rest, "END {")
	if endIdx < 0 {
		t.Fatalf("missing END block in offcpu program: %s", prog)
	}
	switchIn := rest[:endIdx]
	endBlock := rest[endIdx:]

	// First probe (switch-out) must record timestamp, kstack, and comm keyed by prev_pid.
	for _, want := range []string{
		"@start[args.prev_pid] = nsecs;",
		"@kstack[args.prev_pid] = kstack(8);",
		"@comm[args.prev_pid] = comm;",
	} {
		if !strings.Contains(switchOut, want) {
			t.Errorf("switch-out probe missing %q:\n  %s", want, switchOut)
		}
	}

	// Second probe (switch-in) must aggregate saved @kstack/@comm for next_pid
	// (never reading kstack/comm of the unrelated switching-out task) and delete all three maps.
	for _, want := range []string{
		"@[@kstack[args.next_pid], @comm[args.next_pid]] = sum($d);",
		"delete(@start[args.next_pid]);",
		"delete(@kstack[args.next_pid]);",
		"delete(@comm[args.next_pid]);",
	} {
		if !strings.Contains(switchIn, want) {
			t.Errorf("switch-in probe missing %q:\n  %s", want, switchIn)
		}
	}
	if strings.Contains(switchIn, "kstack(8)") || strings.Contains(switchIn, ", comm]") {
		t.Errorf("switch-in probe must not read switching-out task's kstack/comm:\n  %s", switchIn)
	}

	// END block must clear all three auxiliary maps.
	for _, want := range []string{"clear(@start);", "clear(@kstack);", "clear(@comm);"} {
		if !strings.Contains(endBlock, want) {
			t.Errorf("END block missing %q:\n  %s", want, endBlock)
		}
	}
}

func TestPerfFallbackSleepArg(t *testing.T) {
	cases := []struct {
		rem     time.Duration
		wantArg string
		wantOK  bool
	}{
		{850 * time.Millisecond, "0.850", true},
		{100 * time.Millisecond, "0.100", true},
		{2500 * time.Millisecond, "2.500", true},
		{50 * time.Millisecond, "", false},
		{49 * time.Millisecond, "", false},
		{0, "", false},
		{-200 * time.Millisecond, "", false},
	}
	for _, tc := range cases {
		gotArg, gotOK := perfFallbackSleepArg(tc.rem)
		if gotArg != tc.wantArg || gotOK != tc.wantOK {
			t.Errorf("perfFallbackSleepArg(%v) = (%q, %v), want (%q, %v)", tc.rem, gotArg, gotOK, tc.wantArg, tc.wantOK)
		}
	}
}

func TestValidateFilter(t *testing.T) {
	for _, ok := range []string{"", "file", "network,process", "openat,read,write", "!futex"} {
		if err := ValidateFilter(ok); err != nil {
			t.Errorf("ValidateFilter(%q) unexpectedly failed: %v", ok, err)
		}
	}
	for _, bad := range []string{"file; rm -rf /", "$(whoami)", "openat read"} {
		if err := ValidateFilter(bad); err == nil {
			t.Errorf("ValidateFilter(%q) should have failed", bad)
		}
	}
}

func TestSampleParsers(t *testing.T) {
	// 1. /proc/<pid>/task/<tid>/syscall (amd64 & arm64 & running)
	scAmd := ParseProcSyscall("amd64", "202 0xc000100 0x80 0x0 0x0 0x0 0x0 0x7fff10 0x7f1234\n")
	if scAmd == nil || scAmd.Name != "futex" || scAmd.Pc != "0x7f1234" || len(scAmd.Args) != 6 {
		t.Fatalf("unexpected amd64 syscall: %+v", scAmd)
	}
	scArm := ParseProcSyscall("arm64", "63 0x5 0x1000 0x400 0x0 0x0 0x0 0x7ff0 0x401000\n")
	if scArm == nil || scArm.Name != "read" || !SyscallTakesFDArg0(scArm.Name) {
		t.Fatalf("unexpected arm64 syscall: %+v", scArm)
	}
	scRun := ParseProcSyscall("amd64", "running\n")
	if scRun == nil || scRun.Name != "running" || scRun.Number != -1 {
		t.Fatalf("unexpected running syscall: %+v", scRun)
	}

	// 2. schedstat, PSI total, fdinfo detailed (with epoll tfd), /proc/locks (with blocked ->)
	runNs, waitNs, slices := ParseSchedStat("12964880 19600451 10\n")
	if runNs != 12964880 || waitNs != 19600451 || slices != 10 {
		t.Fatalf("unexpected schedstat: %d %d %d", runNs, waitNs, slices)
	}
	someAvg, fullAvg, someTot, fullTot := parsePSIAll("some avg10=1.0 avg60=0.5 avg300=0.1 total=123456\nfull avg10=0.2 avg60=0.1 avg300=0.0 total=45678\n")
	if someAvg != 1.0 || fullAvg != 0.2 || someTot != 123456 || fullTot != 45678 {
		t.Fatalf("unexpected PSI: someAvg=%v fullAvg=%v someTot=%d fullTot=%d", someAvg, fullAvg, someTot, fullTot)
	}
	pos, mode, ino, tfds := ParseFDInfoDetailed("pos:\t1024\nflags:\t02000002\nino:\t554433\ntfd:        9 events:       19 data: 0\ntfd:        4 events:       19 data: 0\n")
	if pos != 1024 || mode != "rw" || ino != 554433 || len(tfds) != 2 || tfds[0] != 4 || tfds[1] != 9 {
		t.Fatalf("unexpected fdinfo detailed: pos=%d mode=%q ino=%d tfds=%v", pos, mode, ino, tfds)
	}
	locks := ParseProcLocks("1: POSIX  ADVISORY  WRITE 100 00:33:33424 0 EOF\n2: -> FLOCK  ADVISORY  WRITE 200 00:33:33424 0 EOF\n")
	if len(locks) != 2 || locks[0].Blocked || locks[0].Inode != 33424 || !locks[1].Blocked || locks[1].Kind != "FLOCK" || locks[1].PID != 200 {
		t.Fatalf("unexpected locks: %+v", locks)
	}

	// 3. Socket tx_queue:rx_queue hex parsing
	sm := map[uint64]SocketInfo{}
	ParseProcNetInet("tcp", "sl local rem st tx_queue rx_queue tr tm retr uid to ino\n0: 0100007F:1F90 0100007F:D428 01 00000400:00000080 00:00 00 1000 0 77777 1\n", sm)
	if sm[77777].TxQueue != 1024 || sm[77777].RxQueue != 128 {
		t.Fatalf("expected tx=1024 rx=128, got %+v", sm[77777])
	}

	// 4. eu-stack & gdb backtrace parsers (including unresolved "- /path/to/bin" module-only lines)
	euOut := `PID 100 - process
TID 100:
#0  0x00007f1111 __syscall_cancel - libc.so.6
#1  0x00007f2222 wait4+0x18 - libc.so.6
    /usr/src/wait4.c:30
#2  0x0000551111 - /usr/bin/python3.14
TID 101:
#0  0x00007f3333 futex_wait - libc.so.6
`
	eu := ParseEuStack(euOut)
	if len(eu[100]) != 3 || eu[100][0].Symbol != "__syscall_cancel" || eu[100][0].Module != "libc.so.6" || eu[100][1].Symbol != "wait4" || eu[100][1].Source != "/usr/src/wait4.c:30" || eu[100][2].Symbol != "python3.14@0x0000551111" || eu[100][2].Module != "python3.14" || len(eu[101]) != 1 {
		t.Fatalf("unexpected ParseEuStack: %+v", eu)
	}
	gdbOut := `Thread 1 (Thread 0x7f00 (LWP 100) "main"):
#0  0x00007f11 in __lll_lock_wait () from /lib/x86_64-linux-gnu/libc.so.6
#1  0x00005522 in main.work () at /work/app/main.go:42
`
	gb := ParseGDBBacktrace(gdbOut)
	if len(gb[100]) != 2 || gb[100][0].Module != "libc.so.6" || gb[100][1].Source != "/work/app/main.go:42" {
		t.Fatalf("unexpected ParseGDBBacktrace: %+v", gb)
	}
}
