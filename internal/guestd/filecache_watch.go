package guestd

import (
	"context"
	"time"
)

const (
	defaultFileCacheThreshold      int64 = 1 << 30 // 1 GiB clean file cache triggers proactive reclaim
	defaultFileCacheCheckInterval        = 10 * time.Second
	defaultFileCacheMinCooldown          = 30 * time.Second
	defaultFileCacheMaxCooldown          = 5 * time.Minute
	defaultFileCacheMinFreed       int64 = 64 << 20  // 64 MiB freed counts as productive reclaim
	defaultFileCacheGrowthOverride int64 = 512 << 20 // 512 MiB new clean cache breaks backoff early
)

type fileCacheWatcher struct {
	srv            *Server
	threshold      int64
	minCooldown    time.Duration
	maxCooldown    time.Duration
	minFreed       int64
	growthOverride int64

	lastAt      time.Time
	cooldown    time.Duration
	lastAfter   int64
	backedOff   bool
}

func (s *Server) newFileCacheWatcher() *fileCacheWatcher {
	threshold := s.fileCacheThreshold
	if threshold <= 0 {
		threshold = defaultFileCacheThreshold
	}
	minCD := s.fileCacheMinCooldown
	if minCD <= 0 {
		minCD = defaultFileCacheMinCooldown
	}
	maxCD := s.fileCacheMaxCooldown
	if maxCD <= 0 {
		maxCD = defaultFileCacheMaxCooldown
	}
	if maxCD < minCD {
		maxCD = minCD
	}
	minFreed := s.fileCacheMinFreed
	if minFreed <= 0 {
		minFreed = defaultFileCacheMinFreed
	}
	growthOverride := defaultFileCacheGrowthOverride
	if growthOverride > threshold {
		growthOverride = threshold / 2
		if growthOverride <= 0 {
			growthOverride = threshold
		}
	}
	return &fileCacheWatcher{
		srv:            s,
		threshold:      threshold,
		minCooldown:    minCD,
		maxCooldown:    maxCD,
		minFreed:       minFreed,
		growthOverride: growthOverride,
		cooldown:       minCD,
	}
}

// step checks the guest's clean file page cache and, when it exceeds the
// threshold and cooldown allows, drops clean page caches (and compacts free
// blocks when at least minFreed bytes were shed) so newly created cladefs
// outputs promote to DAX and return guest RAM to the host before host memory
// pressure builds. Returns true if a reclaim pass ran.
func (w *fileCacheWatcher) step(now time.Time) bool {
	before := w.srv.readCleanFileBytes()
	if before < w.threshold {
		if w.backedOff {
			w.backedOff = false
			w.cooldown = w.minCooldown
		}
		return false
	}
	if !w.lastAt.IsZero() {
		elapsed := now.Sub(w.lastAt)
		if elapsed < w.minCooldown {
			return false
		}
		if elapsed < w.cooldown {
			// Still in backoff after an unproductive drop (e.g. pinned mmap'd
			// files): only retry early if substantial new clean cache built up.
			if before < w.lastAfter+w.growthOverride {
				return false
			}
		}
	}

	if !w.srv.reclaimMu.TryLock() {
		return false
	}
	defer w.srv.reclaimMu.Unlock()

	if !w.srv.hasRunningCommand() {
		w.srv.doSyncFS()
	}
	_ = w.srv.doDropCaches()
	after := w.srv.readCleanFileBytes()
	freed := before - after
	if freed < 0 {
		freed = 0
	}
	if freed >= w.minFreed {
		_ = w.srv.doCompactMemory()
		w.cooldown = w.minCooldown
		w.backedOff = false
		w.srv.log.Info("reclaimed guest file cache",
			"before_bytes", before,
			"after_bytes", after,
			"freed_bytes", freed,
			"threshold_bytes", w.threshold,
		)
	} else {
		next := w.cooldown * 2
		if next > w.maxCooldown {
			next = w.maxCooldown
		}
		w.cooldown = next
		w.backedOff = true
	}
	w.lastAt = now
	w.lastAfter = after
	return true
}

// WatchFileCache periodically monitors the guest's clean file page cache and
// reclaims it once it crosses the configured threshold (default 1 GiB), so
// large builds continuously promote created cladefs outputs to DAX and return
// RAM to the host without waiting for host-wide memory pressure.
func (s *Server) WatchFileCache(ctx context.Context) {
	interval := s.fileCacheCheckInterval
	if interval <= 0 {
		interval = defaultFileCacheCheckInterval
	}
	w := s.newFileCacheWatcher()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.step(s.now())
		}
	}
}
