package guestd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// parseMeminfoSwapTotal parses SwapTotal in bytes from /proc/meminfo text.
func parseMeminfoSwapTotal(raw string) int64 {
	return parseMeminfoField(raw, "SwapTotal:")
}

// parseMeminfoCleanFileBytes parses clean, reclaimable file-backed page cache
// bytes from /proc/meminfo text: Active(file) + Inactive(file) - Dirty - Writeback.
// Using Active(file)+Inactive(file) excludes tmpfs/Shmem (which lives on the
// anonymous LRU in Linux), so tmpfs-heavy workloads never false-trigger reclaim.
func parseMeminfoCleanFileBytes(raw string) int64 {
	activeFile := parseMeminfoField(raw, "Active(file):")
	inactiveFile := parseMeminfoField(raw, "Inactive(file):")
	dirty := parseMeminfoField(raw, "Dirty:")
	writeback := parseMeminfoField(raw, "Writeback:")
	clean := activeFile + inactiveFile - dirty - writeback
	if clean < 0 {
		return 0
	}
	return clean
}

func parseMeminfoField(raw, prefix string) int64 {
	for _, line := range strings.Split(raw, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
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

// reclaimableFileBytes reads a cgroup v2 directory's memory.stat and returns
// the non-anonymous reclaimable bytes (file cache + reclaimable slab) that
// memory.reclaim can shed without swap.
func reclaimableFileBytes(cgDir string) int64 {
	f, err := os.Open(filepath.Join(cgDir, "memory.stat"))
	if err != nil {
		return 0
	}
	defer f.Close()

	var file, inactiveFile, activeFile, slabReclaimable int64
	var hasFile bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch k {
		case "file":
			file = n
			hasFile = true
		case "inactive_file":
			inactiveFile = n
		case "active_file":
			activeFile = n
		case "slab_reclaimable":
			slabReclaimable = n
		}
	}
	if !hasFile {
		file = inactiveFile + activeFile
	}
	return file + slabReclaimable
}

// planReclaim computes the payload to write to a command cgroup's
// memory.reclaim file. In a swapless guest (swapTotal <= 0), anonymous memory
// cannot be paged out, so asking memory.reclaim for anonymous bytes makes the
// kernel spin scanning unreclaimable anon LRU lists before returning EAGAIN.
// When swapTotal == 0, clamp to the cgroup's reclaimable file/slab bytes with
// swappiness=0 — and skip the write altogether when nothing is reclaimable.
func planReclaim(cgDir string, requested, swapTotal int64) (payload string, ok bool) {
	if requested <= 0 {
		return "", false
	}
	if swapTotal > 0 {
		return strconv.FormatInt(requested, 10), true
	}
	avail := reclaimableFileBytes(cgDir)
	if avail <= 0 {
		return "", false
	}
	target := min(requested, avail)
	return fmt.Sprintf("%d swappiness=0", target), true
}
