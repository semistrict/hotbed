package main

import (
	"strconv"
	"strings"
)

const (
	zramMinDiskSizeBytes = 64 << 20 // 64 MiB floor
	zramPageSizeBytes    = 4096
)

var zramPreferredAlgorithms = []string{"zstd", "lz4", "lzo-rle", "lzo"}

// parseMeminfoMemTotal parses MemTotal in bytes from /proc/meminfo contents.
func parseMeminfoMemTotal(raw string) int64 {
	for _, line := range strings.Split(raw, "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || n <= 0 {
			return 0
		}
		if len(fields) >= 2 && strings.EqualFold(fields[1], "kB") {
			return n * 1024
		}
		return n
	}
	return 0
}

// computeZramDiskSize sizes the guest zram swap device to 100% of guest RAM
// (rounded down to a 4 KiB page boundary, with a 64 MiB floor for valid RAM).
// With typical 3-4x compression on build workloads, a 100% RAM disksize gives
// roughly 25-33% guest RAM worst-case footprint while absorbing memory spikes
// and enabling in-guest cgroup memory.reclaim.
func computeZramDiskSize(memTotalBytes int64) int64 {
	if memTotalBytes <= 0 {
		return 0
	}
	size := (memTotalBytes / zramPageSizeBytes) * zramPageSizeBytes
	if size < zramMinDiskSizeBytes {
		size = zramMinDiskSizeBytes
	}
	return size
}

// selectZramCompAlgorithm chooses the best available compression algorithm
// from /sys/block/zram0/comp_algorithm output (e.g. "lzo [lzo-rle] lz4 zstd").
func selectZramCompAlgorithm(raw string) string {
	avail := map[string]bool{}
	for _, tok := range strings.Fields(raw) {
		name := strings.Trim(tok, "[]")
		if name != "" {
			avail[name] = true
		}
	}
	for _, pref := range zramPreferredAlgorithms {
		if avail[pref] {
			return pref
		}
	}
	return ""
}

// swapActiveForDevice reports whether /proc/swaps already lists devPath.
func swapActiveForDevice(procSwaps, devPath string) bool {
	for i, line := range strings.Split(procSwaps, "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == devPath {
			return true
		}
	}
	return false
}
