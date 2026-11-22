package unit

import (
	"os"
	"path/filepath"
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

// A bind's target on the host cannot be removed as an empty directory once a
// unit was given it: removing it would detach the binds on it in every
// running command's namespace (board 6114).
func TestPinMountpointKeepsTheTargetFromRemoval(t *testing.T) {
	target := filepath.Join(t.TempDir(), "work")
	for range 2 { // the second unit's start finds it pinned
		if err := pinMountpoint(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(target); err == nil {
		t.Fatalf("removed %s, a pinned mountpoint", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != PinFile {
		t.Errorf("%s holds %v, want only %s", target, entries, PinFile)
	}
}
