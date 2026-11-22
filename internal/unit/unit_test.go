package unit

import (
	"strings"
	"testing"
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
			"LoadState=loaded\nActiveState=failed\nSubState=failed\nExecMainStatus=9\nExecMainCode=2\n",
			State{Loaded: true, Exited: true, Signal: 9},
		},
		{"gone", "LoadState=not-found\nActiveState=inactive\n", State{}},
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

func TestDescriptionIsOneShortLine(t *testing.T) {
	got := description([]string{"bash", "-c", "set -e\ncd /work\n\techo hi"})
	if got != "hotbed bash -c set -e cd /work echo hi" {
		t.Errorf("description = %q", got)
	}
	long := description([]string{strings.Repeat("x", 300)})
	if n := len([]rune(long)); n != len("hotbed ")+200 || !strings.HasSuffix(long, "…") {
		t.Errorf("a long command's description has %d runes: %q", n, long)
	}
}
