package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
)

const (
	defaultBaseRetireGrace  = 5 * time.Minute
	retiredBasesFile        = "retired-bases.json"
	baseBuildUploadInterval = 15 * time.Second
	// captureUploadInterval is the maximum cladefsd per-cell upload interval
	// (24h, cladefs cells.rs MAX_UPLOAD_INTERVAL_MS). Capture-enabled workspace
	// cells use it so writes stay on local SSD while commands run and only the
	// surviving net extents flush to R2 if a command succeeds and captures.
	captureUploadInterval = 24 * time.Hour
)

type retiredBase struct {
	Tenant      uint64    `json:"tenant"`
	Cell        uint64    `json:"cell"`
	BaseID      string    `json:"baseId,omitempty"`
	RetireAfter time.Time `json:"retireAfter"`
}

// baseRetirer tracks this node's superseded base image cells and deletes them
// after a short grace window so forks under way finish before the old cell
// head is marked deleted for cladefs-gc (hotbed question 537). Project bases
// are not its: the controller retires those (HeartbeatResponse.retire_bases).
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
		// Project bases are the controller's to retire now (another node may
		// hold one this node replaced): keep only base images.
		for _, b := range saved {
			if strings.HasPrefix(b.BaseID, "baseimage-") {
				r.pending = append(r.pending, b)
			} else {
				slog.Info("leaving project base cell to the controller to retire", "baseId", b.BaseID, "cell", b.Cell)
			}
		}
	}
}

func (r *baseRetirer) saveLocked() {
	if r.cacheDir == "" {
		return
	}
	if err := writeJSONAtomic(filepath.Join(r.cacheDir, retiredBasesFile), r.pending); err != nil {
		slog.Warn("save the base cells waiting to retire", "dir", r.cacheDir, "err", err)
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
// return immediately so two callers never collide.
func (r *baseRetirer) sweep(ctx context.Context, store *cladefs.Store, now time.Time) {
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
		if err := store.DeleteBucketCell(ctx, entry.Tenant, entry.Cell); err != nil {
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
