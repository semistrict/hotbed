package unit

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseShow(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want State
	}{
		{
			"running",
			"LoadState=loaded\nActiveState=active\nSubState=running\nExecMainStatus=0\nExecMainCode=0\nMainPID=4242\nControlGroup=/hotbed.slice/hotbed-c-1.service\nCPUUsageNSec=1500000000\nMemoryCurrent=104857600\nFreezerState=running\n",
			State{Loaded: true, Running: true, PID: 4242, Cgroup: "/hotbed.slice/hotbed-c-1.service", CPUNanos: 1500000000, Memory: 104857600},
		},
		{
			"frozen",
			"LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=4242\nFreezerState=frozen\n",
			State{Loaded: true, Running: true, Frozen: true, PID: 4242},
		},
		{
			"exited 3",
			"LoadState=loaded\nActiveState=failed\nSubState=failed\nExecMainStatus=3\nExecMainCode=1\nMainPID=0\n",
			State{Loaded: true, Exited: true, ExitCode: 3},
		},
		{
			"exited 0, kept by RemainAfterExit",
			"LoadState=loaded\nActiveState=active\nSubState=exited\nExecMainStatus=0\nExecMainCode=1\nMainPID=0\n",
			State{Loaded: true, Exited: true},
		},
		{
			"killed by SIGKILL",
			"LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=signal\nExecMainStatus=9\nExecMainCode=2\n",
			State{Loaded: true, Exited: true, Result: "signal", Signal: 9},
		},
		{
			"OOM kill with OOMPolicy=stop SIGTERM on main PID",
			"LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=oom-kill\nExecMainStatus=15\nExecMainCode=2\n",
			State{Loaded: true, Exited: true, Result: "oom-kill", OOMKilled: true, Signal: 15},
		},
		{
			"OOM kill with non-zero main exit code",
			"LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=oom-kill\nExecMainStatus=101\nExecMainCode=1\n",
			State{Loaded: true, Exited: true, Result: "oom-kill", OOMKilled: true, ExitCode: 101},
		},
		{"gone", "LoadState=not-found\nActiveState=inactive\n", State{}},
		{
			"deactivating while frozen (e.g. OOMPolicy=stop)",
			"LoadState=loaded\nActiveState=deactivating\nSubState=final-sigterm\nFreezerState=frozen\n",
			State{Loaded: true, Running: false, Frozen: true, Exited: false},
		},
		{
			"loaded inactive dead",
			"LoadState=loaded\nActiveState=inactive\nSubState=dead\nExecMainStatus=0\nExecMainCode=1\n",
			State{Loaded: true, Exited: true},
		},
	} {
		got, err := parseShow(tc.out)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseShowMany(t *testing.T) {
	out := strings.Join([]string{
		// Leading blank line should be ignored.
		"",
		"Id=hotbed-run.service",
		"LoadState=loaded",
		"ActiveState=active",
		"SubState=running",
		"Result=success",
		"ExecMainStatus=0",
		"ExecMainCode=0",
		"MainPID=4242",
		"ControlGroup=/hotbed.slice/hotbed-run=1.service",
		"Description=cmd a=b=c",
		"CPUUsageNSec=1500000000",
		"MemoryCurrent=104857600",
		"FreezerState=running",
		"",
		"", // Multiple blank lines between blocks.
		"Id=hotbed-missing.service",
		"LoadState=not-found",
		"ActiveState=inactive",
		"SubState=dead",
		"",
		"Id=hotbed-oom.service",
		"LoadState=loaded",
		"ActiveState=failed",
		"SubState=failed",
		"Result=oom-kill",
		"ExecMainStatus=15",
		"ExecMainCode=2",
		"MainPID=0",
		"FreezerState=running",
		"",
		// Block without Id falls back to positional matching.
		"LoadState=loaded",
		"ActiveState=active",
		"SubState=running",
		"MainPID=9001",
		"FreezerState=frozen",
		"",
	}, "\n")

	names := []string{"hotbed-run", "hotbed-missing", "hotbed-oom", "hotbed-pos"}
	got, err := parseShowMany(out, names)
	if err != nil {
		t.Fatalf("parseShowMany: %v", err)
	}
	want := map[string]State{
		"hotbed-run": {
			Loaded:   true,
			Running:  true,
			Result:   "success",
			PID:      4242,
			Cgroup:   "/hotbed.slice/hotbed-run=1.service",
			CPUNanos: 1500000000,
			Memory:   104857600,
		},
		"hotbed-missing": {},
		"hotbed-oom": {
			Loaded:    true,
			Exited:    true,
			Result:    "oom-kill",
			OOMKilled: true,
			Signal:    15,
		},
		"hotbed-pos": {
			Loaded:  true,
			Running: true,
			Frozen:  true,
			PID:     9001,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d units (%+v), want %d", len(got), got, len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s:\n got %+v (present=%v)\nwant %+v", k, g, ok, w)
		}
	}
}

func TestStatusManyEmpty(t *testing.T) {
	got, err := StatusMany(t.Context(), nil)
	if err != nil {
		t.Fatalf("StatusMany(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("StatusMany(nil) = %#v, want empty non-nil map", got)
	}
	got, err = StatusMany(t.Context(), []string{})
	if err != nil {
		t.Fatalf("StatusMany([]): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("StatusMany([]) = %#v, want empty non-nil map", got)
	}
}

func TestStatusManyAtEmpty(t *testing.T) {
	before := time.Now()
	got, sampledAt, err := StatusManyAt(t.Context(), nil)
	after := time.Now()
	if err != nil {
		t.Fatalf("StatusManyAt(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("StatusManyAt(nil) = %#v, want empty non-nil map", got)
	}
	if sampledAt.Before(before) || sampledAt.After(after) {
		t.Fatalf("StatusManyAt(nil) sampledAt = %v, want between %v and %v", sampledAt, before, after)
	}
}

func TestParseMemoryEventsOOM(t *testing.T) {
	if parseMemoryEventsOOM("low 0\nhigh 12\nmax 3\noom 0\noom_kill 0\noom_group_kill 0\n") {
		t.Error("expected false when oom_kill and oom_group_kill are 0")
	}
	if !parseMemoryEventsOOM("low 0\nhigh 12\noom 1\noom_kill 1\noom_group_kill 0\n") {
		t.Error("expected true when oom_kill > 0")
	}
	if !parseMemoryEventsOOM("oom_kill 0\noom_group_kill 2\n") {
		t.Error("expected true when oom_group_kill > 0")
	}
	if parseMemoryEventsOOM("") {
		t.Error("expected false on empty input")
	}
}

func TestSliceDir(t *testing.T) {
	for slice, want := range map[string]string{
		"hotbed.slice":         "/hotbed.slice",
		"user-hotbed.slice":    "/user.slice/user-hotbed.slice",
		"a-b-c.slice":          "/a.slice/a-b.slice/a-b-c.slice",
		"system-getty_x.slice": "/system.slice/system-getty_x.slice",
	} {
		if got := SliceDir(slice); got != want {
			t.Errorf("SliceDir(%s) = %s, want %s", slice, got, want)
		}
	}
	for _, bad := range []string{"hotbed", "-x.slice", "a--b.slice", "a/b.slice", ".slice"} {
		if CheckSlice(bad) == nil {
			t.Errorf("CheckSlice(%q) passed", bad)
		}
	}
	if err := CheckSlice("user-hotbed.slice"); err != nil {
		t.Error(err)
	}
}

// startArgs rejects a Spec without RootDir: every command runs inside a rootfs.
func TestStartArgsRequiresRootDir(t *testing.T) {
	if _, err := startArgs(Spec{Name: "hotbed-c", Argv: []string{"true"}}); err == nil {
		t.Fatal("startArgs accepted empty RootDir, want error")
	}
}

func TestStartArgsRootDir(t *testing.T) {
	args, err := startArgs(Spec{
		Name:          "hotbed-c-1",
		RootDir:       "/var/lib/hotbed/vol/ws/abc",
		Dir:           "/work/cladefs",
		User:          "ramon",
		Argv:          []string{"cargo", "test"},
		Log:           "/var/lib/hotbed/logs/c-1.log",
		PathPrefix:    []string{"/work/.hotbed/bin", "/work/.hotbed/cargo/bin"},
		Tmp:           "/var/lib/hotbed/vol/ws/abc/.tmp/c-1",
		Binds:         []Bind{{Source: "/mnt/scratch", Target: "/mnt/scratch"}},
		ReadOnlyBinds: []Bind{{Source: "/host/ro", Target: "/etc/ro"}},
		Env:           []string{"HOME=/work/.hotbed/home"},
		Capabilities:  []string{"CAP_PERFMON"},
		TraceFS:       true,
		MemoryHigh:    1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--property=RootDirectory=/var/lib/hotbed/vol/ws/abc",
		"--property=MountAPIVFS=yes",
		"--property=BindReadOnlyPaths=-/etc/resolv.conf",
		"--property=BindPaths=-/sys/kernel/tracing:/sys/kernel/tracing",
		"--property=BindPaths=-/sys/fs/cgroup:/sys/fs/cgroup",
		"--uid=ramon",
		"--property=AmbientCapabilities=CAP_PERFMON",
		"--working-directory=/work/cladefs",
		"--property=BindPaths=/mnt/scratch:/mnt/scratch",
		"--property=BindPaths=/var/lib/hotbed/vol/ws/abc/.tmp/c-1:/tmp",
		"--property=BindPaths=/var/lib/hotbed/vol/ws/abc/.tmp/c-1:/var/tmp",
		"--property=BindReadOnlyPaths=/host/ro:/etc/ro",
		"--property=MemoryHigh=1073741824",
		"--setenv=TMPDIR=/tmp",
		"--setenv=HOME=/work/.hotbed/home",
		"--setenv=PATH=/work/.hotbed/bin:/work/.hotbed/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"/bin/sh",
		`exec "$@"`,
		"cargo",
		"test",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("startArgs missing %q in %v", want, args)
		}
	}
	// Never set NoNewPrivileges=yes or Delegate=yes: passwordless sudo to real
	// root must work inside the chroot.
	for _, forbid := range []string{"-lc", "/bin/bash", "--property=NoNewPrivileges=yes", "--property=Delegate=yes", "--property=PrivatePIDs=yes"} {
		if slices.Contains(args, forbid) {
			t.Errorf("startArgs in RootDir mode must not contain %q (args=%v)", forbid, args)
		}
	}
}

// Every command is the OOM killer's first pick, whatever else it asks for.
func TestStartArgsMakeCommandsTheOOMKillersFirstPick(t *testing.T) {
	for _, s := range []Spec{
		{Name: "hotbed-c", Argv: []string{"true"}, RootDir: "/vol/ws/w"},
		{Name: "hotbed-c", Argv: []string{"true"}, RootDir: "/vol/ws/w", NoSwap: true, TraceFS: true},
	} {
		if args, err := startArgs(s); err != nil || !slices.Contains(args, "--property=OOMScoreAdjust=1000") {
			t.Errorf("startArgs(%+v) = %q, err=%v; want OOMScoreAdjust=1000", s, args, err)
		}
	}
}

// On SELinux hosts, transient command service units run in
// unconfined_u:unconfined_r:unconfined_t:s0 so programs like rsync do not
// transition into confined domains (board 7921). On hosts without SELinux,
// SELinuxContext is omitted.
func TestStartArgsSELinuxContext(t *testing.T) {
	const wantProp = "--property=SELinuxContext=" + unconfinedSELinuxContext
	spec := Spec{Name: "hotbed-c-1", Argv: []string{"rsync", "-a", "a", "b"}, RootDir: "/vol/ws/w", User: "ramon", Log: "/var/lib/hotbed/logs/c-1.log"}

	origSELinux := hasSELinux
	t.Cleanup(func() { hasSELinux = origSELinux })

	hasSELinux = func() bool { return true }
	args, err := startArgs(spec)
	if err != nil {
		t.Fatalf("selinux=true: %v", err)
	}
	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("selinux=true: missing -- in %q", args)
	}
	if !slices.Contains(args[:sep], wantProp) {
		t.Errorf("selinux=true: missing %q in %q", wantProp, args[:sep])
	}

	hasSELinux = func() bool { return false }
	args, err = startArgs(spec)
	if err != nil {
		t.Fatalf("selinux=false: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "SELinuxContext") {
			t.Errorf("selinux=false: unexpected SELinuxContext arg %q in %q", a, args)
		}
	}
}

// Exec runs in a transient systemd scope, which has no ExecContext and rejects
// SELinuxContext= with "Unknown assignment". Verify it is never emitted even
// when SELinux is enabled (board 7921).
func TestExecScopeOmitsSELinuxContext(t *testing.T) {
	fakeHost(t)
	origSELinux := hasSELinux
	hasSELinux = func() bool { return true }
	t.Cleanup(func() { hasSELinux = origSELinux })

	cmd, err := Exec(t.Context(), ExecPrefix+"test", "", ExecSpec{
		Root: "/vol/ws/w",
		Dir:  "/work",
		Argv: []string{"git", "status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "SELinuxContext") {
			t.Errorf("Exec scope must not pass SELinuxContext (systemd-run --scope rejects it): %q", cmd.Args)
		}
	}
}

func TestExecSpecValidate(t *testing.T) {
	ok := ExecSpec{Root: "/vol/ws/a", Dir: "/work", Argv: []string{"true"}, ReadOnlyBinds: []Bind{{Source: "/etc/resolv.conf", Target: "/etc/resolv.conf"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]ExecSpec{
		"relative root": {Root: "vol", Dir: "/", Argv: []string{"true"}},
		"root is /":     {Root: "/", Dir: "/", Argv: []string{"true"}},
		"no command":    {Root: "/r", Dir: "/"},
		"relative dir":  {Root: "/r", Dir: "work", Argv: []string{"true"}},
		"bind onto /":   {Root: "/r", Dir: "/", Argv: []string{"true"}, Binds: []Bind{{Source: "/x", Target: "/"}}},
		"relative src":  {Root: "/r", Dir: "/", Argv: []string{"true"}, ReadOnlyBinds: []Bind{{Source: "x", Target: "/x"}}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
}

// fakeHost stubs host lookups for unit tests on non-Linux hosts.
func fakeHost(t *testing.T) {
	t.Helper()
	lp, ru := lookPath, resolveUser
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	resolveUser = func(name string) (resolvedUser, error) {
		if name == "" {
			return resolvedUser{}, nil
		}
		return resolvedUser{set: true, uid: 1000, gid: 1000, groups: []string{"1000", "27"}}, nil
	}
	t.Cleanup(func() { lookPath, resolveUser = lp, ru })
}

func TestParseListAndSliceUnits(t *testing.T) {
	listOut := strings.Join([]string{
		"hotbed-worker.service   loaded active running Hotbed Worker",
		"hotbed-c-1.service      loaded active running hotbed sleep 30",
		"hotbed-c-2.service      loaded failed failed  hotbed false",
		"● hotbed-deadbeef.service loaded failed failed hotbed crash",
		"hotbed-b-1.service      loaded active running hotbed cargo bench",
		"unrelated.service       loaded active running watches hotbed-ignored.service",
		"",
	}, "\n")
	cands := parseListUnits(listOut)
	wantCands := []string{"hotbed-worker.service", "hotbed-c-1.service", "hotbed-c-2.service", "hotbed-deadbeef.service", "hotbed-b-1.service"}
	if !slices.Equal(cands, wantCands) {
		t.Fatalf("parseListUnits = %q, want %q", cands, wantCands)
	}

	showOut := strings.Join([]string{
		"Id=hotbed-worker.service",
		"Slice=system.slice",
		"LoadState=loaded",
		"ActiveState=active",
		"",
		"Id=hotbed-c-1.service",
		"Slice=hotbed.slice",
		"LoadState=loaded",
		"ActiveState=active",
		"",
		"Id=hotbed-c-2.service",
		"Slice=hotbed.slice",
		"LoadState=loaded",
		"ActiveState=failed",
		"",
		"Id=hotbed-c-dead.service",
		"Slice=hotbed.slice",
		"LoadState=loaded",
		"ActiveState=inactive",
		"",
		"Id=hotbed-b-1.service",
		"Slice=hotbed_bench.slice",
		"LoadState=loaded",
		"ActiveState=active",
		"",
	}, "\n")
	got := parseSliceUnits(showOut, DefaultSlice)
	want := []string{"hotbed-c-1", "hotbed-c-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("parseSliceUnits(hotbed.slice) = %q, want %q", got, want)
	}
	gotBench := parseSliceUnits(showOut, "hotbed_bench.slice")
	if !slices.Equal(gotBench, []string{"hotbed-b-1"}) {
		t.Fatalf("parseSliceUnits(hotbed_bench.slice) = %q, want [hotbed-b-1]", gotBench)
	}
}

// A unit's own memory is its anonymous memory and swap, not the file cache
// its cgroup also holds: admission reserves it, and cache is not needed.
func TestMemoryIsAnonAndSwapNotCache(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.swap.current", "2048\n")
	write("memory.stat", "anon_thp 0\nanon 4096\nfile 999999\ninactive_file 5000\n")
	m, err := memoryIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MemoryUse{Anon: 4096, Swap: 2048}); m != want || m.Held() != 6144 {
		t.Fatalf("memory = %+v (held %d), want %+v (held 6144)", m, m.Held(), want)
	}
	write("memory.stat", "file 1\n")
	if _, err := memoryIn(dir); err == nil || !strings.Contains(err.Error(), "no anon") {
		t.Fatalf("memory.stat without anon: err = %v, want no anon", err)
	}
}

// A work slice leaves the host an eighth of its RAM, at least 1 GiB: past
// High the slice is throttled, past Max one of its commands is killed,
// before the host runs short (board 9128). On hosts with zram swap, swap
// lives in host RAM outside the slice's memcg, so SwapMax bounds it and
// High/Max reserve the full 1:1 worst-case zram footprint (board 9361, 9588).
func TestBackstopLeavesTheHostRoom(t *testing.T) {
	const GiB = 1 << 30
	const MiB = 1 << 20
	for _, c := range []struct {
		name      string
		total     int64
		swap      Swap
		want      Backstop
		wantProps []string
	}{
		{
			name:      "no swap 28 GiB",
			total:     28 * GiB,
			want:      Backstop{Headroom: 3584 * MiB, High: 25088 * MiB, Max: 26880 * MiB, SwapMax: SwapMaxUnbounded},
			wantProps: []string{"MemoryHigh=26306674688", "MemoryMax=28185722880", "MemorySwapMax=infinity"},
		},
		{
			name:      "no swap 4 GiB",
			total:     4 * GiB,
			want:      Backstop{Headroom: 1 * GiB, High: 3 * GiB, Max: 3584 * MiB, SwapMax: SwapMaxUnbounded},
			wantProps: []string{"MemoryHigh=3221225472", "MemoryMax=3758096384", "MemorySwapMax=infinity"},
		},
		{
			name:      "disk-only swap leaves SwapMax unbounded (vm1)",
			total:     12 * GiB,
			swap:      Swap{Disk: 24 * GiB},
			want:      Backstop{Headroom: 1536 * MiB, High: 10752 * MiB, Max: 11520 * MiB, SwapMax: SwapMaxUnbounded},
			wantProps: []string{"MemoryHigh=11274289152", "MemoryMax=12079595520", "MemorySwapMax=infinity"},
		},
		{
			name:      "zram-only swap 16 GiB preserves full headroom at 1:1",
			total:     16 * GiB,
			swap:      Swap{Zram: 8 * GiB},
			want:      Backstop{Headroom: 2 * GiB, High: 12 * GiB, Max: 13 * GiB, SwapMax: 2 * GiB},
			wantProps: []string{"MemoryHigh=12884901888", "MemoryMax=13958643712", "MemorySwapMax=2147483648"},
		},
		{
			name:      "zram-only swap 17 GiB (asahimbp)",
			total:     17 * GiB,
			swap:      Swap{Zram: 8 * GiB},
			want:      Backstop{Headroom: 2176 * MiB, High: 13056 * MiB, Max: 14144 * MiB, SwapMax: 2176 * MiB},
			wantProps: []string{"MemoryHigh=13690208256", "MemoryMax=14831058944", "MemorySwapMax=2281701376"},
		},
		{
			name:      "mixed zram and disk with large zram caps SwapMax 28 GiB (ubu1-like)",
			total:     28 * GiB,
			swap:      Swap{Zram: 14 * GiB, Disk: 8 * GiB},
			want:      Backstop{Headroom: 3584 * MiB, High: 21504 * MiB, Max: 23296 * MiB, SwapMax: 3584 * MiB},
			wantProps: []string{"MemoryHigh=22548578304", "MemoryMax=24427626496", "MemorySwapMax=3758096384"},
		},
		{
			name:      "mixed small zram within budget and disk leaves SwapMax unbounded",
			total:     32 * GiB,
			swap:      Swap{Zram: 1 * GiB, Disk: 16 * GiB},
			want:      Backstop{Headroom: 4 * GiB, High: 27 * GiB, Max: 29 * GiB, SwapMax: SwapMaxUnbounded},
			wantProps: []string{"MemoryHigh=28991029248", "MemoryMax=31138512896", "MemorySwapMax=infinity"},
		},
		{
			name:      "small zram-only host caps SwapMax to device size",
			total:     32 * GiB,
			swap:      Swap{Zram: 1 * GiB},
			want:      Backstop{Headroom: 4 * GiB, High: 27 * GiB, Max: 29 * GiB, SwapMax: 1 * GiB},
			wantProps: []string{"MemoryHigh=28991029248", "MemoryMax=31138512896", "MemorySwapMax=1073741824"},
		},
		{
			name:      "small 4 GiB VM with huge zram",
			total:     4 * GiB,
			swap:      Swap{Zram: 16 * GiB},
			want:      Backstop{Headroom: 1 * GiB, High: 2304 * MiB, Max: 2816 * MiB, SwapMax: 768 * MiB},
			wantProps: []string{"MemoryHigh=2415919104", "MemoryMax=2952790016", "MemorySwapMax=805306368"},
		},
		{
			name:      "small 2 GiB VM with zram keeps High positive",
			total:     2 * GiB,
			swap:      Swap{Zram: 2 * GiB},
			want:      Backstop{Headroom: 1 * GiB, High: 768 * MiB, Max: 1280 * MiB, SwapMax: 256 * MiB},
			wantProps: []string{"MemoryHigh=805306368", "MemoryMax=1342177280", "MemorySwapMax=268435456"},
		},
		{
			name:      "tiny 1 GiB VM clamps headroom safely",
			total:     1 * GiB,
			swap:      Swap{Zram: 1 * GiB},
			want:      Backstop{Headroom: 512 * MiB, High: 384 * MiB, Max: 640 * MiB, SwapMax: 128 * MiB},
			wantProps: []string{"MemoryHigh=402653184", "MemoryMax=671088640", "MemorySwapMax=134217728"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := BackstopFor(c.total, c.swap)
			if got != c.want {
				t.Errorf("BackstopFor(%d, %+v) = %+v, want %+v", c.total, c.swap, got, c.want)
			}
			props := got.Properties()
			if !slices.Equal(props, c.wantProps) {
				t.Errorf("Properties() = %q, want %q", props, c.wantProps)
			}
			// Unbounded SwapMax must explicitly emit MemorySwapMax=infinity so
			// stale finite runtime caps are cleared on worker restart, never
			// MemorySwapMax=0 (which disables swap).
			if got.SwapMax == SwapMaxUnbounded {
				if !slices.Contains(props, "MemorySwapMax=infinity") {
					t.Errorf("unbounded backstop Properties() = %q, want MemorySwapMax=infinity", props)
				}
			}
			// Invariants: High < Max < total, and even in the 1:1 incompressible
			// worst case High+zramBound leaves full Headroom and Max+zramBound
			// leaves at least Headroom/2 hard host reserve.
			if !(0 < got.High && got.High < got.Max && got.Max < c.total) {
				t.Errorf("broken ordering: High=%d Max=%d total=%d", got.High, got.Max, c.total)
			}
			if c.swap.Zram > 0 {
				zramBound := c.swap.Zram
				if got.SwapMax > 0 {
					zramBound = min(zramBound, got.SwapMax)
				}
				if reserve := c.total - (got.High + zramBound); reserve < got.Headroom {
					t.Errorf("at 1:1 incompressible zram, total-(High+zramBound) = %d, want >= Headroom (%d)",
						reserve, got.Headroom)
				}
				if reserve := c.total - (got.Max + zramBound); reserve < got.Headroom/2 {
					t.Errorf("at 1:1 incompressible zram, total-(Max+zramBound) = %d, want >= Headroom/2 (%d)",
						reserve, got.Headroom/2)
				}
				if got.SwapMax > 0 {
					if reserve := c.total - (got.High + got.SwapMax); reserve < got.Headroom {
						t.Errorf("at 1:1 incompressible worst case, total-(High+SwapMax) = %d, want >= Headroom (%d)",
							reserve, got.Headroom)
					}
					if reserve := c.total - (got.Max + got.SwapMax); reserve < got.Headroom/2 {
						t.Errorf("at 1:1 incompressible worst case, total-(Max+SwapMax) = %d, want >= Headroom/2 (%d)",
							reserve, got.Headroom/2)
					}
				}
			}
		})
	}
}

func TestBackstopClampRespectsStaticLimits(t *testing.T) {
	const GiB = 1 << 30
	base := BackstopFor(28*GiB, Swap{Zram: 14 * GiB, Disk: 8 * GiB})

	// Tighter host drop-in (e.g. ubu1's 157-memory_guard.sh: High=16G, Max=18G, SwapMax=4G)
	// must tighten High and Max while keeping the tighter SwapMax (3.5 GiB).
	got := base.Clamp(MemoryLimits{High: 16 * GiB, Max: 18 * GiB, SwapMax: 4 * GiB})
	if got.High != 16*GiB || got.Max != 18*GiB || got.SwapMax != base.SwapMax {
		t.Fatalf("Clamp(16G/18G/4G) = %+v, want High=16G Max=18G SwapMax=%d", got, base.SwapMax)
	}

	// Tighter High alone also pulls Max down to High + Headroom/2.
	highOnly := base.Clamp(MemoryLimits{High: 16 * GiB})
	wantMax := 16*GiB + base.Headroom/2
	if highOnly.High != 16*GiB || highOnly.Max != wantMax {
		t.Fatalf("Clamp(High=16G) = %+v, want High=16G Max=%d", highOnly, wantMax)
	}

	// Looser static limits never weaken the computed backstop.
	looser := base.Clamp(MemoryLimits{High: 26 * GiB, Max: 27 * GiB, SwapMax: 8 * GiB})
	if looser != base {
		t.Fatalf("Clamp(looser) = %+v, want unchanged %+v", looser, base)
	}

	// Tighter SwapMax clamps an unbounded (disk-only) backstop too.
	disk := BackstopFor(12*GiB, Swap{Disk: 24 * GiB})
	clampedDisk := disk.Clamp(MemoryLimits{SwapMax: 2 * GiB})
	if clampedDisk.SwapMax != 2*GiB {
		t.Fatalf("Clamp(SwapMax=2G on disk-only) = %+v, want SwapMax=2G", clampedDisk)
	}
}

func TestParseMemoryLimitFiles(t *testing.T) {
	const GiB = 1 << 30
	dir := t.TempDir()
	f1 := filepath.Join(dir, "10-base.conf")
	f2 := filepath.Join(dir, "50-guard.conf")
	if err := os.WriteFile(f1, []byte("[Slice]\nMemoryHigh=20G\nMemoryMax=24G\nMemorySwapMax=6G\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("[Unit]\nDescription=guard\n[Slice]\nMemoryHigh=16G\nMemoryMax=18G\nMemorySwapMax=4G\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parseMemoryLimitFiles([]string{f1, f2}, 28*GiB)
	want := MemoryLimits{High: 16 * GiB, Max: 18 * GiB, SwapMax: 4 * GiB}
	if got != want {
		t.Fatalf("parseMemoryLimitFiles = %+v, want %+v", got, want)
	}
}
