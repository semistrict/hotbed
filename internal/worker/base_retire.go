package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
)

const (
	defaultBaseRetireGrace  = 5 * time.Minute
	retiredBasesFile        = "retired-bases.json"
	baseBuildUploadInterval = 15 * time.Second
)

type retiredBase struct {
	Tenant      uint64    `json:"tenant"`
	Cell        uint64    `json:"cell"`
	BaseID      string    `json:"baseId,omitempty"`
	RetireAfter time.Time `json:"retireAfter"`
}

// baseRetirer tracks superseded R2 base cells and deletes them after a short
// grace window so concurrent forks from other workers finish before the old
// cell head is marked deleted for cladefs-gc (hotbed question 537).
type baseRetirer struct {
	mu       sync.Mutex
	sweepMu  sync.Mutex
	grace    time.Duration // <=0 uses defaultBaseRetireGrace; set explicit tiny/negative via override
	graceSet bool
	cacheDir string
	pending  []retiredBase
}

func (r *baseRetirer) setGrace(d time.Duration) {
	r.mu.Lock()
	r.grace = d
	r.graceSet = true
	r.mu.Unlock()
}

func (r *baseRetirer) gracePeriodLocked() time.Duration {
	if r.graceSet {
		return r.grace
	}
	return defaultBaseRetireGrace
}

func (r *baseRetirer) load(cacheDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheDir = cacheDir
	if cacheDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, retiredBasesFile))
	if err != nil {
		return
	}
	var saved []retiredBase
	if json.Unmarshal(raw, &saved) == nil {
		r.pending = saved
	}
}

func (r *baseRetirer) saveLocked() {
	if r.cacheDir == "" {
		return
	}
	if err := os.MkdirAll(r.cacheDir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(r.pending, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(r.cacheDir, retiredBasesFile)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// enqueue schedules (tenant, cell) for bucket deletion after the grace period.
// Duplicate entries are ignored so the original RetireAfter is preserved.
func (r *baseRetirer) enqueue(tenant, cell uint64, baseID string, now time.Time) {
	if cell == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.pending {
		if p.Tenant == tenant && p.Cell == cell {
			return
		}
	}
	r.pending = append(r.pending, retiredBase{
		Tenant:      tenant,
		Cell:        cell,
		BaseID:      baseID,
		RetireAfter: now.Add(r.gracePeriodLocked()),
	})
	r.saveLocked()
}

// sweep deletes any pending retired base cells whose grace window has elapsed.
// Cells that fail transiently stay queued for the next sweep. Concurrent sweeps
// return immediately so two callers never collide on the same staging directory.
func (r *baseRetirer) sweep(ctx context.Context, store *cladefs.Store, stagingPrefix string, now time.Time) {
	if store == nil {
		return
	}
	if !r.sweepMu.TryLock() {
		return
	}
	defer r.sweepMu.Unlock()
	r.mu.Lock()
	var due []retiredBase
	for _, p := range r.pending {
		if !p.RetireAfter.After(now) {
			due = append(due, p)
		}
	}
	r.mu.Unlock()
	if len(due) == 0 {
		return
	}

	deleted := make(map[[2]uint64]bool, len(due))
	for _, entry := range due {
		rel := filepath.Join(stagingPrefix, fmt.Sprintf("%d-%d", entry.Tenant, entry.Cell))
		if err := store.DeleteBucketCell(ctx, rel, entry.Tenant, entry.Cell); err != nil {
			slog.Warn("retire superseded R2 base cell failed; will retry",
				"baseId", entry.BaseID, "tenant", entry.Tenant, "cell", entry.Cell, "err", err)
			continue
		}
		slog.Info("retired superseded R2 base cell",
			"baseId", entry.BaseID, "tenant", entry.Tenant, "cell", entry.Cell)
		deleted[[2]uint64{entry.Tenant, entry.Cell}] = true
	}
	if len(deleted) == 0 {
		return
	}

	r.mu.Lock()
	kept := r.pending[:0]
	for _, p := range r.pending {
		if !deleted[[2]uint64{p.Tenant, p.Cell}] {
			kept = append(kept, p)
		}
	}
	r.pending = kept
	r.saveLocked()
	r.mu.Unlock()
}

type supersededBaseImage struct {
	key string
	ref cladefs.CellRef
}

// collectSupersededBaseImages removes every cached base image other than keepKey
// (with a distinct non-zero cell ID) from baseImages and returns them for
// cache-file cleanup and deferred R2 retirement. Caller must hold the map lock.
func collectSupersededBaseImages(baseImages map[string]cladefs.CellRef, keepKey string, keepCell uint64) []supersededBaseImage {
	var out []supersededBaseImage
	for oldKey, oldRef := range baseImages {
		if oldKey != keepKey && oldRef.Cell > 0 && oldRef.Cell != keepCell {
			out = append(out, supersededBaseImage{key: oldKey, ref: oldRef})
			delete(baseImages, oldKey)
		}
	}
	return out
}

// retireSupersededBaseImages removes stale baseimage-<key>.json cache files and
// enqueues their R2 cells for deferred deletion after the grace window.
func retireSupersededBaseImages(cacheDir string, superseded []supersededBaseImage, retirer *baseRetirer, now time.Time) {
	for _, old := range superseded {
		if cacheDir != "" {
			_ = os.Remove(filepath.Join(cacheDir, "baseimage-"+old.key+".json"))
		}
		retirer.enqueue(old.ref.Tenant, old.ref.Cell, "baseimage-"+old.key, now)
	}
}
