package inspect

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseProcStat(t *testing.T) {
	// Comm contains spaces and parentheses; parser must find the LAST ')'.
	line := "12345 (my (weird) proc) S 100 12345 100 0 -1 4194304 10 0 0 0 250 75 0 0 20 0 4 0 9999 104857600 4096 18446744073709551615"
	got, err := ParseProcStat(line)
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	want := ProcStat{
		PID:     12345,
		Comm:    "my (weird) proc",
		State:   "S",
		PPID:    100,
		UTime:   250,
		STime:   75,
		Threads: 4,
		VSize:   104857600,
		RSS:     4096,
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
	pos, mode := ParseFDInfo("pos:\t4096\nflags:\t02000002\nmnt_id:\t42\n")
	if pos != 4096 || mode != "rw" {
		t.Fatalf("got pos=%d mode=%q", pos, mode)
	}
}

func TestParseCgroupAndStacksAndLimits(t *testing.T) {
	some, full := ParsePSI("some avg10=1.25 avg60=0.50 avg300=0.10 total=1234\nfull avg10=0.40 avg60=0.10 avg300=0.01 total=50\n")
	if some != 1.25 || full != 0.40 {
		t.Fatalf("unexpected PSI: some=%v full=%v", some, full)
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
	stacks, fns := ParsePerfScript(perfScriptOut, 20)
	if len(stacks) != 2 || len(fns) < 4 {
		t.Fatalf("unexpected perf script stacks=%+v fns=%+v", stacks, fns)
	}
	if stacks[0].Stack != "compile;ssa.Compile;ssa.regalloc" && stacks[1].Stack != "compile;ssa.Compile;ssa.regalloc" {
		t.Fatalf("expected root->leaf folded stack, got %+v", stacks)
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
