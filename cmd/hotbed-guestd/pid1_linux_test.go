//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"testing"
)

func TestPivotToWorkspaceRootUnsetIsNoop(t *testing.T) {
	called := false
	ops := pivotRootOps{
		mount: func(string, string, string, uintptr, string) error {
			called = true
			return nil
		},
	}
	pivoted, devMoved, err := pivotToWorkspaceRootWithOps(nil, "", ops)
	if err != nil || pivoted || devMoved || called {
		t.Fatalf("got (%v, %v, %v, called=%v), want (false, false, nil, called=false)", pivoted, devMoved, err, called)
	}
}

func TestPivotToWorkspaceRootSequence(t *testing.T) {
	var trace []string
	ops := pivotRootOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			trace = append(trace, fmt.Sprintf("mount:%s:%s:%s:%x:%s", source, target, fstype, flags, data))
			return nil
		},
		mkdirAll: func(path string, perm os.FileMode) error {
			trace = append(trace, fmt.Sprintf("mkdir:%s:%o", path, perm))
			return nil
		},
		pivotRoot: func(newroot, putold string) error {
			trace = append(trace, fmt.Sprintf("pivot:%s:%s", newroot, putold))
			return nil
		},
		chdir: func(dir string) error {
			trace = append(trace, "chdir:"+dir)
			return nil
		},
		unsetenv: func(key string) error {
			trace = append(trace, "unsetenv:"+key)
			return nil
		},
	}
	pivoted, devMoved, err := pivotToWorkspaceRootWithOps(nil, "cladefs:rootfs", ops)
	if err != nil || !pivoted || !devMoved {
		t.Fatalf("got (%v, %v, %v), want (true, true, nil)", pivoted, devMoved, err)
	}
	want := []string{
		fmt.Sprintf("mount::/::%x:", syscall.MS_PRIVATE|syscall.MS_REC),
		"mount:rootfs:/newroot:cladefs:0:",
		"mkdir:/newroot/dev:755",
		fmt.Sprintf("mount:/dev:/newroot/dev::%x:", syscall.MS_MOVE),
		"mkdir:/newroot/run:755",
		fmt.Sprintf("mount:tmpfs:/newroot/run:tmpfs:%x:mode=755", syscall.MS_NOSUID|syscall.MS_NODEV),
		"mkdir:/newroot/run/hotbed-tools:555",
		"pivot:/newroot:/newroot/run/hotbed-tools",
		"chdir:/",
		"unsetenv:HOTBED_ROOT",
	}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace =\n%v\nwant =\n%v", trace, want)
	}
}

func TestPivotToWorkspaceRootDevMoveFallbackAndInvalidSpec(t *testing.T) {
	ops := pivotRootOps{
		mount: func(source, target, fstype string, flags uintptr, data string) error {
			if flags == syscall.MS_MOVE {
				return errors.New("not a mount point")
			}
			return nil
		},
		mkdirAll:  func(string, os.FileMode) error { return nil },
		pivotRoot: func(string, string) error { return nil },
		chdir:     func(string) error { return nil },
		unsetenv:  func(string) error { return nil },
	}
	pivoted, devMoved, err := pivotToWorkspaceRootWithOps(nil, "cladefs:rootfs", ops)
	if err != nil || !pivoted || devMoved {
		t.Fatalf("got (%v, %v, %v), want (true, false, nil)", pivoted, devMoved, err)
	}
	if _, _, err := pivotToWorkspaceRootWithOps(nil, "bogus-without-colon", ops); err == nil {
		t.Fatal("expected error for invalid HOTBED_ROOT")
	}
}

func TestConfigureGuestWriteback(t *testing.T) {
	procRoot := t.TempDir()
	vmDir := procRoot + "/sys/vm"
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configureGuestWriteback(nil, procRoot)

	want := map[string]string{
		"dirty_bytes":               "536870912\n",
		"dirty_background_bytes":    "67108864\n",
		"dirty_expire_centisecs":    "500\n",
		"dirty_writeback_centisecs": "200\n",
	}
	for name, wantVal := range want {
		got, err := os.ReadFile(vmDir + "/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != wantVal {
			t.Fatalf("%s = %q, want %q", name, string(got), wantVal)
		}
	}
}
