//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type zramConfig struct {
	sysDir      string
	devPath     string
	meminfoPath string
	swapsPath   string
	vmSysctlDir string
	runCmd      func(name string, args ...string) error
}

func defaultZramConfig() zramConfig {
	return zramConfig{
		sysDir:      "/sys/block/zram0",
		devPath:     "/dev/zram0",
		meminfoPath: "/proc/meminfo",
		swapsPath:   "/proc/swaps",
		vmSysctlDir: "/proc/sys/vm",
		runCmd: func(name string, args ...string) error {
			bin, err := exec.LookPath(name)
			if err != nil {
				return err
			}
			out, err := exec.Command(bin, args...).CombinedOutput()
			if err != nil {
				msg := strings.TrimSpace(string(out))
				if msg != "" {
					return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, msg)
				}
				return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
			}
			return nil
		},
	}
}

// setupGuestZram configures /dev/zram0 as always-on compressed swap inside the
// guest VM when the kernel exposes CONFIG_ZRAM. Older kernels without zram
// continue cleanly without error.
func setupGuestZram(log *slog.Logger) bool {
	return setupGuestZramWith(log, defaultZramConfig())
}

func setupGuestZramWith(log *slog.Logger, cfg zramConfig) bool {
	if _, err := os.Stat(cfg.sysDir); err != nil {
		if os.IsNotExist(err) {
			if log != nil {
				log.Info("guest zram unavailable in kernel; continuing without guest swap", "sysDir", cfg.sysDir)
			}
			return false
		}
		if log != nil {
			log.Warn("stat guest zram sysfs failed", "sysDir", cfg.sysDir, "err", err)
		}
		return false
	}

	if b, err := os.ReadFile(cfg.swapsPath); err == nil && swapActiveForDevice(string(b), cfg.devPath) {
		tuneZramSysctls(log, cfg.vmSysctlDir)
		return true
	}

	meminfoBytes, err := os.ReadFile(cfg.meminfoPath)
	if err != nil {
		if log != nil {
			log.Warn("read guest meminfo for zram sizing failed", "path", cfg.meminfoPath, "err", err)
		}
		return false
	}
	diskSize := computeZramDiskSize(parseMeminfoMemTotal(string(meminfoBytes)))
	if diskSize <= 0 {
		if log != nil {
			log.Warn("could not determine guest MemTotal for zram sizing", "path", cfg.meminfoPath)
		}
		return false
	}

	diskSizePath := filepath.Join(cfg.sysDir, "disksize")
	resetPath := filepath.Join(cfg.sysDir, "reset")
	if b, err := os.ReadFile(diskSizePath); err == nil {
		if cur, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil && cur > 0 {
			if err := os.WriteFile(resetPath, []byte("1\n"), 0o644); err != nil && log != nil {
				log.Warn("reset pre-configured zram0 failed", "path", resetPath, "err", err)
			}
		}
	}

	var algo string
	compPath := filepath.Join(cfg.sysDir, "comp_algorithm")
	if b, err := os.ReadFile(compPath); err == nil {
		if algo = selectZramCompAlgorithm(string(b)); algo != "" {
			if err := os.WriteFile(compPath, []byte(algo+"\n"), 0o644); err != nil && log != nil {
				log.Warn("set zram compression algorithm failed", "algo", algo, "err", err)
			}
		}
	}

	if err := os.WriteFile(diskSizePath, []byte(strconv.FormatInt(diskSize, 10)+"\n"), 0o644); err != nil {
		if log != nil {
			log.Warn("set zram disksize failed", "disksize", diskSize, "err", err)
		}
		return false
	}

	if err := cfg.runCmd("mkswap", cfg.devPath); err != nil {
		_ = os.WriteFile(resetPath, []byte("1\n"), 0o644)
		if log != nil {
			log.Warn("mkswap on zram failed", "device", cfg.devPath, "err", err)
		}
		return false
	}
	if err := cfg.runCmd("swapon", "-p", "100", cfg.devPath); err != nil {
		_ = os.WriteFile(resetPath, []byte("1\n"), 0o644)
		if log != nil {
			log.Warn("swapon on zram failed", "device", cfg.devPath, "err", err)
		}
		return false
	}

	tuneZramSysctls(log, cfg.vmSysctlDir)
	if log != nil {
		log.Info("enabled guest zram swap", "device", cfg.devPath, "disksizeBytes", diskSize, "algorithm", algo)
	}
	return true
}

func tuneZramSysctls(log *slog.Logger, vmDir string) {
	// Keep hot anonymous working sets in RAM under normal operation (swappiness=10)
	// while still letting OOM pressure and explicit cgroup memory.reclaim compress
	// cold pages into zram. page-cluster=0 disables swap readahead, which only
	// wastes CPU/RAM on random-access compressed memory.
	for _, kv := range []struct{ file, val string }{
		{"swappiness", "10\n"},
		{"page-cluster", "0\n"},
	} {
		p := filepath.Join(vmDir, kv.file)
		if err := os.WriteFile(p, []byte(kv.val), 0o644); err != nil && !os.IsNotExist(err) && log != nil {
			log.Warn("set vm sysctl for zram failed", "path", p, "val", strings.TrimSpace(kv.val), "err", err)
		}
	}
}
