//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSetupGuestZramUnavailableKernel(t *testing.T) {
	tmp := t.TempDir()
	var cmds [][]string
	ok := setupGuestZramWith(nil, zramConfig{
		sysDir:      filepath.Join(tmp, "missing-zram0"),
		devPath:     "/dev/zram0",
		meminfoPath: filepath.Join(tmp, "meminfo"),
		swapsPath:   filepath.Join(tmp, "swaps"),
		vmSysctlDir: tmp,
		runCmd: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
	})
	if ok {
		t.Fatal("expected setupGuestZramWith to return false when zram sysfs is absent")
	}
	if len(cmds) != 0 {
		t.Fatalf("unexpected commands invoked: %v", cmds)
	}
}

func TestSetupGuestZramHappyPathAndTuning(t *testing.T) {
	tmp := t.TempDir()
	sysDir := filepath.Join(tmp, "zram0")
	vmDir := filepath.Join(tmp, "vm")
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meminfoPath := filepath.Join(tmp, "meminfo")
	swapsPath := filepath.Join(tmp, "swaps")
	if err := os.WriteFile(meminfoPath, []byte("MemTotal:        8388608 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(swapsPath, []byte("Filename Type Size Used Priority\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysDir, "comp_algorithm"), []byte("lzo [lzo-rle] lz4 zstd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysDir, "disksize"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var cmds [][]string
	ok := setupGuestZramWith(nil, zramConfig{
		sysDir:      sysDir,
		devPath:     "/dev/zram0",
		meminfoPath: meminfoPath,
		swapsPath:   swapsPath,
		vmSysctlDir: vmDir,
		runCmd: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
	})
	if !ok {
		t.Fatal("expected setupGuestZramWith to succeed")
	}

	gotAlgo, err := os.ReadFile(filepath.Join(sysDir, "comp_algorithm"))
	if err != nil || strings.TrimSpace(string(gotAlgo)) != "zstd" {
		t.Fatalf("comp_algorithm = %q (%v), want zstd", string(gotAlgo), err)
	}
	gotSize, err := os.ReadFile(filepath.Join(sysDir, "disksize"))
	wantSize := strconv.FormatInt(8<<30, 10)
	if err != nil || strings.TrimSpace(string(gotSize)) != wantSize {
		t.Fatalf("disksize = %q (%v), want %s", string(gotSize), err, wantSize)
	}
	wantCmds := [][]string{
		{"mkswap", "/dev/zram0"},
		{"swapon", "-p", "100", "/dev/zram0"},
	}
	if !reflect.DeepEqual(cmds, wantCmds) {
		t.Fatalf("commands = %v, want %v", cmds, wantCmds)
	}
	if b, err := os.ReadFile(filepath.Join(vmDir, "swappiness")); err != nil || strings.TrimSpace(string(b)) != "10" {
		t.Fatalf("swappiness = %q (%v), want 10", string(b), err)
	}
	if b, err := os.ReadFile(filepath.Join(vmDir, "page-cluster")); err != nil || strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("page-cluster = %q (%v), want 0", string(b), err)
	}
}

func TestSetupGuestZramIdempotentAndResetOnFailure(t *testing.T) {
	t.Run("already active skips mkswap and swapon", func(t *testing.T) {
		tmp := t.TempDir()
		sysDir := filepath.Join(tmp, "zram0")
		vmDir := filepath.Join(tmp, "vm")
		_ = os.MkdirAll(sysDir, 0o755)
		_ = os.MkdirAll(vmDir, 0o755)
		swapsPath := filepath.Join(tmp, "swaps")
		_ = os.WriteFile(swapsPath, []byte("Filename Type Size Used Priority\n/dev/zram0 partition 8388604 0 100\n"), 0o644)

		var ran bool
		ok := setupGuestZramWith(nil, zramConfig{
			sysDir:      sysDir,
			devPath:     "/dev/zram0",
			meminfoPath: filepath.Join(tmp, "meminfo"),
			swapsPath:   swapsPath,
			vmSysctlDir: vmDir,
			runCmd: func(string, ...string) error {
				ran = true
				return nil
			},
		})
		if !ok {
			t.Fatal("expected true when /dev/zram0 is already active")
		}
		if ran {
			t.Fatal("did not expect mkswap/swapon when already active")
		}
		if b, err := os.ReadFile(filepath.Join(vmDir, "swappiness")); err != nil || strings.TrimSpace(string(b)) != "10" {
			t.Fatalf("swappiness = %q (%v), want 10", string(b), err)
		}
	})

	t.Run("resets preconfigured disksize and resets on swapon failure", func(t *testing.T) {
		tmp := t.TempDir()
		sysDir := filepath.Join(tmp, "zram0")
		vmDir := filepath.Join(tmp, "vm")
		_ = os.MkdirAll(sysDir, 0o755)
		_ = os.MkdirAll(vmDir, 0o755)
		meminfoPath := filepath.Join(tmp, "meminfo")
		swapsPath := filepath.Join(tmp, "swaps")
		_ = os.WriteFile(meminfoPath, []byte("MemTotal:        4194304 kB\n"), 0o644)
		_ = os.WriteFile(swapsPath, []byte("Filename Type Size Used Priority\n"), 0o644)
		_ = os.WriteFile(filepath.Join(sysDir, "disksize"), []byte("1048576\n"), 0o644)
		_ = os.WriteFile(filepath.Join(sysDir, "comp_algorithm"), []byte("[lz4]\n"), 0o644)

		ok := setupGuestZramWith(nil, zramConfig{
			sysDir:      sysDir,
			devPath:     "/dev/zram0",
			meminfoPath: meminfoPath,
			swapsPath:   swapsPath,
			vmSysctlDir: vmDir,
			runCmd: func(name string, _ ...string) error {
				if name == "swapon" {
					return errors.New("swapon EBUSY")
				}
				return nil
			},
		})
		if ok {
			t.Fatal("expected false on swapon failure")
		}
		if b, err := os.ReadFile(filepath.Join(sysDir, "reset")); err != nil || strings.TrimSpace(string(b)) != "1" {
			t.Fatalf("expected reset=1 after swapon failure, got %q (%v)", string(b), err)
		}
	})
}
