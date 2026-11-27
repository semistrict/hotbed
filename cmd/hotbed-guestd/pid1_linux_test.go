//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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
		rootIsInitramfs: func() (bool, error) { return false, nil },
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

// Under machined hotbed-guestd boots from an initramfs, which pivot_root(2)
// refuses to leave: it moves the workspace root over / and chroots instead,
// keeping the initramfs at /run/hotbed-tools.
func TestPivotToWorkspaceRootFromInitramfs(t *testing.T) {
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
			return syscall.EINVAL
		},
		chdir: func(dir string) error {
			trace = append(trace, "chdir:"+dir)
			return nil
		},
		chroot: func(dir string) error {
			trace = append(trace, "chroot:"+dir)
			return nil
		},
		unsetenv: func(key string) error {
			trace = append(trace, "unsetenv:"+key)
			return nil
		},
		rootIsInitramfs: func() (bool, error) { return true, nil },
	}
	pivoted, devMoved, err := pivotToWorkspaceRootWithOps(nil, "cladefs:root", ops)
	if err != nil || !pivoted || !devMoved {
		t.Fatalf("got (%v, %v, %v), want (true, true, nil)", pivoted, devMoved, err)
	}
	want := []string{
		fmt.Sprintf("mount::/::%x:", syscall.MS_PRIVATE|syscall.MS_REC),
		"mount:root:/newroot:cladefs:0:",
		"mkdir:/newroot/dev:755",
		fmt.Sprintf("mount:/dev:/newroot/dev::%x:", syscall.MS_MOVE),
		"mkdir:/newroot/run:755",
		fmt.Sprintf("mount:tmpfs:/newroot/run:tmpfs:%x:mode=755", syscall.MS_NOSUID|syscall.MS_NODEV),
		"mkdir:/newroot/run/hotbed-tools:555",
		fmt.Sprintf("mount:/:/newroot/run/hotbed-tools::%x:", syscall.MS_BIND),
		"chdir:/newroot",
		fmt.Sprintf("mount:/newroot:/::%x:", syscall.MS_MOVE),
		"chroot:.",
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
		mkdirAll:        func(string, os.FileMode) error { return nil },
		pivotRoot:       func(string, string) error { return nil },
		chdir:           func(string) error { return nil },
		unsetenv:        func(string) error { return nil },
		rootIsInitramfs: func() (bool, error) { return false, nil },
	}
	pivoted, devMoved, err := pivotToWorkspaceRootWithOps(nil, "cladefs:rootfs", ops)
	if err != nil || !pivoted || devMoved {
		t.Fatalf("got (%v, %v, %v), want (true, false, nil)", pivoted, devMoved, err)
	}
	if _, _, err := pivotToWorkspaceRootWithOps(nil, "bogus-without-colon", ops); err == nil {
		t.Fatal("expected error for invalid HOTBED_ROOT")
	}
}

func TestConfigureGuestSysctls(t *testing.T) {
	procRoot := t.TempDir()
	for _, d := range []string{"/sys/vm", "/sys/kernel"} {
		if err := os.MkdirAll(procRoot+d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configureGuestSysctls(nil, procRoot)

	want := map[string]string{
		"vm/dirty_bytes":               "536870912\n",
		"vm/dirty_background_bytes":    "67108864\n",
		"vm/dirty_expire_centisecs":    "500\n",
		"vm/dirty_writeback_centisecs": "200\n",
		"kernel/perf_event_paranoid":   "-1\n",
		"kernel/kptr_restrict":         "0\n",
	}
	for name, wantVal := range want {
		got, err := os.ReadFile(procRoot + "/sys/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != wantVal {
			t.Fatalf("%s = %q, want %q", name, string(got), wantVal)
		}
	}
}

func TestSyncGuestClock(t *testing.T) {
	hostSec := int64(1795738800)

	// Guest clock at epoch 1970 (no RTC) -> steps to hostSec and unsets env.
	var setTo int64
	var unset []string
	syncGuestClockWithOps(nil, fmt.Sprint(hostSec), func() time.Time { return time.Unix(12, 0) }, func(sec int64) error {
		setTo = sec
		return nil
	}, func(k string) error {
		unset = append(unset, k)
		return nil
	})
	if setTo != hostSec {
		t.Fatalf("setTo = %d, want %d", setTo, hostSec)
	}
	if !slices.Equal(unset, []string{"HOTBED_NOW"}) {
		t.Fatalf("unset = %v, want [HOTBED_NOW]", unset)
	}

	// Guest clock already within 5s -> no-op.
	called := false
	syncGuestClockWithOps(nil, fmt.Sprint(hostSec), func() time.Time { return time.Unix(hostSec-2, 0) }, func(int64) error {
		called = true
		return nil
	}, func(string) error { return nil })
	if called {
		t.Fatal("expected no clock step when skew is within 5s")
	}

	// Invalid/empty HOTBED_NOW -> no-op.
	syncGuestClockWithOps(nil, "not-a-timestamp", func() time.Time { return time.Unix(0, 0) }, func(int64) error {
		called = true
		return nil
	}, func(string) error { return nil })
	if called {
		t.Fatal("expected no clock step for invalid HOTBED_NOW")
	}
}

func TestConfigureGuestMGLRU(t *testing.T) {
	sysRoot := t.TempDir()
	lruDir := filepath.Join(sysRoot, "kernel", "mm", "lru_gen")
	if err := os.MkdirAll(lruDir, 0o755); err != nil {
		t.Fatal(err)
	}
	enabledPath := filepath.Join(lruDir, "enabled")
	ttlPath := filepath.Join(lruDir, "min_ttl_ms")
	if err := os.WriteFile(enabledPath, []byte("0x0000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ttlPath, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	configureGuestMGLRU(nil, sysRoot)

	if got, _ := os.ReadFile(enabledPath); string(got) != "y\n" {
		t.Fatalf("lru_gen/enabled = %q, want %q", got, "y\n")
	}
	if got, _ := os.ReadFile(ttlPath); string(got) != "1000\n" {
		t.Fatalf("lru_gen/min_ttl_ms = %q, want %q", got, "1000\n")
	}

	// Already-enabled MGLRU and non-zero min_ttl_ms are left untouched.
	if err := os.WriteFile(enabledPath, []byte("0x0007\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ttlPath, []byte("500\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configureGuestMGLRU(nil, sysRoot)
	if got, _ := os.ReadFile(enabledPath); string(got) != "0x0007\n" {
		t.Fatalf("already-enabled lru_gen/enabled overwritten: %q", got)
	}
	if got, _ := os.ReadFile(ttlPath); string(got) != "500\n" {
		t.Fatalf("non-zero min_ttl_ms overwritten: %q", got)
	}
}

func TestBuildLinuxSwapHeader(t *testing.T) {
	for _, pageSize := range []int{4096, 16384} {
		diskSize := int64(256 << 20)
		hdr, err := buildLinuxSwapHeader(diskSize, pageSize)
		if err != nil {
			t.Fatalf("buildLinuxSwapHeader(%d): %v", pageSize, err)
		}
		if len(hdr) != pageSize {
			t.Fatalf("len(hdr) = %d, want %d", len(hdr), pageSize)
		}
		if got := binary.LittleEndian.Uint32(hdr[1024:1028]); got != 1 {
			t.Errorf("version = %d, want 1", got)
		}
		wantLast := uint32(diskSize/int64(pageSize) - 1)
		if got := binary.LittleEndian.Uint32(hdr[1028:1032]); got != wantLast {
			t.Errorf("last_page = %d, want %d", got, wantLast)
		}
		if got := string(hdr[pageSize-10 : pageSize]); got != "SWAPSPACE2" {
			t.Errorf("magic = %q, want SWAPSPACE2", got)
		}
	}
}

func TestConfigureGuestZramSwap(t *testing.T) {
	t.Run("provisions zram with lz4 fallback on EINVAL", func(t *testing.T) {
		var writes []string
		var formattedDev string
		var formattedSize int64
		var swaponDev string
		var swaponFlags int
		ops := zramSwapOps{
			readFile: func(path string) ([]byte, error) {
				switch path {
				case "/proc/meminfo":
					return []byte("MemTotal:       18874368 kB\nSwapTotal:             0 kB\n"), nil
				case "/sys/class/zram-control/hot_add":
					return []byte("0\n"), nil
				default:
					return nil, os.ErrNotExist
				}
			},
			writeFile: func(path string, data []byte, _ os.FileMode) error {
				val := strings.TrimSpace(string(data))
				writes = append(writes, path+"="+val)
				if path == "/sys/block/zram0/comp_algorithm" && val == "zstd" {
					return syscall.EINVAL
				}
				return nil
			},
			stat:     func(string) (os.FileInfo, error) { return nil, nil },
			modprobe: func(string) error { return errors.New("unexpected modprobe") },
			formatSwap: func(devPath string, diskSize int64, _ int) error {
				formattedDev, formattedSize = devPath, diskSize
				return nil
			},
			swapon: func(devPath string, flags int) error {
				swaponDev, swaponFlags = devPath, flags
				return nil
			},
			pageSize:   func() int { return 4096 },
			waitForDev: func(string) error { return nil },
		}
		if err := configureGuestZramSwapWithOps(nil, ops); err != nil {
			t.Fatalf("configureGuestZramSwapWithOps: %v", err)
		}
		wantSize := int64(9 << 30) // half of 18 GiB
		if formattedDev != "/dev/zram0" || formattedSize != wantSize {
			t.Fatalf("formatSwap = (%q, %d), want (/dev/zram0, %d)", formattedDev, formattedSize, wantSize)
		}
		if swaponDev != "/dev/zram0" || swaponFlags != swapFlagPrefer|(zramSwapPriority<<swapFlagPrioShift) {
			t.Fatalf("swapon = (%q, %#x)", swaponDev, swaponFlags)
		}
		wantWrites := []string{
			"/sys/block/zram0/comp_algorithm=zstd",
			"/sys/block/zram0/comp_algorithm=lz4",
			fmt.Sprintf("/sys/block/zram0/disksize=%d", wantSize),
		}
		if !slices.Equal(writes, wantWrites) {
			t.Fatalf("writes = %v, want %v", writes, wantWrites)
		}
	})

	t.Run("skips when swap already present", func(t *testing.T) {
		ops := zramSwapOps{
			readFile: func(path string) ([]byte, error) {
				if path == "/proc/meminfo" {
					return []byte("MemTotal:       4194304 kB\nSwapTotal:      1048576 kB\n"), nil
				}
				t.Fatalf("unexpected read %s", path)
				return nil, nil
			},
		}
		if err := configureGuestZramSwapWithOps(nil, ops); err != nil {
			t.Fatalf("configureGuestZramSwapWithOps: %v", err)
		}
	})

	t.Run("cleans up zram device via hot_remove on swapon failure", func(t *testing.T) {
		var removed string
		ops := zramSwapOps{
			readFile: func(path string) ([]byte, error) {
				switch path {
				case "/proc/meminfo":
					return []byte("MemTotal:       4194304 kB\nSwapTotal:             0 kB\n"), nil
				case "/sys/class/zram-control/hot_add":
					return []byte("3\n"), nil
				default:
					return nil, os.ErrNotExist
				}
			},
			writeFile: func(path string, data []byte, _ os.FileMode) error {
				if path == "/sys/class/zram-control/hot_remove" {
					removed = strings.TrimSpace(string(data))
				}
				return nil
			},
			stat:       func(string) (os.FileInfo, error) { return nil, nil },
			formatSwap: func(string, int64, int) error { return nil },
			swapon:     func(string, int) error { return syscall.EPERM },
			pageSize:   func() int { return 4096 },
			waitForDev: func(string) error { return nil },
		}
		if err := configureGuestZramSwapWithOps(nil, ops); err == nil {
			t.Fatal("expected swapon error")
		}
		if removed != "3" {
			t.Fatalf("hot_remove = %q, want %q", removed, "3")
		}
	})
}
