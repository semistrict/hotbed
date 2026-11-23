package worker

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/semistrict/hotbed/internal/storage"
	"github.com/semistrict/hotbed/internal/unit"
	"golang.org/x/sync/errgroup"
)

// sweepOrphans cleans up worker-owned units, volumes, and snapshots that are
// not referenced by the loaded worker database (for example, after worker.db
// is lost and the worker restarts with a fresh database; board 7923).
//
// It runs at startup after e.load() and before the worker accepts RPCs or
// starts new base builds, in strict dependency order:
//  1. Stop orphaned hotbed-* units in the worker's slice(s) so open FDs and
//     mount namespaces release orphaned volumes.
//  2. Destroy orphaned workspace volumes ("ws/<id>") so ZFS clones are removed
//     before destroying orphaned base volumes/snapshots.
//  3. Destroy orphaned base volumes ("bases/<project>/g<N>"), highest
//     generation first to avoid unnecessary ZFS promotions.
//  4. Destroy orphaned base snapshots (excluding "baseimg-*" snapshots owned by
//     baseimage.Manager).
//
// Shared base-image volumes ("baseimages/...") and snapshots ("baseimg-*") and
// their provisioning units ("hotbed-baseimg-*") are managed by
// baseimage.Manager and are never swept here.
func (e *Engine) sweepOrphans(ctx context.Context) {
	knownUnits := make(map[string]bool)
	liveWorkspaceVols := make(map[string]bool)
	knownBaseVols := make(map[string]bool)
	knownBaseSnaps := make(map[string]bool)

	e.mu.Lock()
	for _, c := range e.commands {
		knownUnits[c.unitName()] = true
	}
	for _, b := range e.bases {
		if b.Command != "" {
			knownUnits["hotbed-"+b.Command] = true
		}
		knownBaseVols[baseVolume(b.Project, b.Generation)] = true
		knownBaseSnaps[b.Id] = true
		knownBaseSnaps[baseSnapshot(b.Project, b.Generation)] = true
	}
	for _, w := range e.workspaces {
		if w.ws().State == released {
			continue
		}
		if w.rec.Volume != "" {
			liveWorkspaceVols[w.rec.Volume] = true
		}
		liveWorkspaceVols["ws/"+w.ws().Id] = true
		if w.rec.BaseSnapshot != "" {
			knownBaseSnaps[w.rec.BaseSnapshot] = true
		}
	}
	e.mu.Unlock()

	// 1. Stop orphaned units concurrently (bounded) before destroying volumes
	// so no running process holds an orphaned volume busy.
	if e.cfg.Units != nil {
		var g errgroup.Group
		g.SetLimit(8)
		for _, slice := range e.orphanSlices() {
			units, err := e.cfg.Units.List(ctx, slice)
			if err != nil {
				slog.Warn("sweep orphans: list units", "slice", slice, "err", err)
				continue
			}
			for _, u := range units {
				if !isEngineOwnedUnit(u) || knownUnits[u] {
					continue
				}
				g.Go(func() error {
					if err := e.cfg.Units.Stop(ctx, u); err != nil {
						slog.Warn("sweep orphan unit failed", "unit", u, "slice", slice, "err", err)
						return nil
					}
					slog.Info("swept orphan unit", "unit", u, "slice", slice)
					return nil
				})
			}
		}
		_ = g.Wait()
	}

	if e.cfg.Store == nil {
		return
	}

	vols, err := e.cfg.Store.ListVolumes(ctx)
	if err != nil {
		slog.Warn("sweep orphans: list volumes", "err", err)
	} else {
		type baseVol struct {
			name    string
			project string
			gen     int
		}
		var orphanWS []string
		var orphanBases []baseVol
		for _, v := range vols {
			if isWorkspaceVolume(v) {
				if !liveWorkspaceVols[v] {
					orphanWS = append(orphanWS, v)
				}
				continue
			}
			if proj, gen, ok := parseBaseVolume(v); ok {
				if !knownBaseVols[v] {
					orphanBases = append(orphanBases, baseVol{name: v, project: proj, gen: gen})
				}
			}
		}

		// 2. Destroy orphaned workspace volumes BEFORE base volumes and snapshots
		// so ZFS clones depending on orphaned base snapshots are removed first.
		for _, v := range orphanWS {
			if err := e.cfg.Store.Destroy(ctx, v); err != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					slog.Warn("sweep orphan workspace volume failed", "volume", v, "err", err)
				}
				continue
			}
			slog.Info("swept orphan workspace volume", "volume", v)
		}

		// 3. Destroy orphaned base volumes, newest generation first per project
		// so dependent generations go before their parent snapshots on ZFS.
		slices.SortFunc(orphanBases, func(a, b baseVol) int {
			if c := strings.Compare(a.project, b.project); c != 0 {
				return c
			}
			return b.gen - a.gen
		})
		for _, b := range orphanBases {
			if err := e.cfg.Store.Destroy(ctx, b.name); err != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					slog.Warn("sweep orphan base volume failed", "volume", b.name, "err", err)
				}
				continue
			}
			slog.Info("swept orphan base volume", "volume", b.name)
		}
	}

	// 4. Destroy orphaned base snapshots (listed after volume removal so
	// snapshots already removed with their datasets on ZFS are not re-visited).
	snaps, err := e.cfg.Store.ListSnapshots(ctx)
	if err != nil {
		slog.Warn("sweep orphans: list snapshots", "err", err)
		return
	}
	for _, snap := range snaps {
		if strings.HasPrefix(snap, "baseimg-") || knownBaseSnaps[snap] {
			continue
		}
		if err := e.cfg.Store.DestroySnapshot(ctx, snap); err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				slog.Warn("sweep orphan base snapshot failed", "snapshot", snap, "err", err)
			}
			continue
		}
		slog.Info("swept orphan base snapshot", "snapshot", snap)
	}
}

func (e *Engine) orphanSlices() []string {
	primary := cmp.Or(e.cfg.Slice, unit.DefaultSlice)
	out := []string{primary}
	if e.cfg.Bench != nil {
		if b := e.cfg.Bench.Slice(); b != "" && b != primary {
			out = append(out, b)
		}
	}
	return out
}

// isEngineOwnedUnit reports whether name is a command/freshen unit owned by
// Engine (as opposed to the worker daemon itself or baseimage provisioning
// units, which baseimage.Manager owns).
func isEngineOwnedUnit(name string) bool {
	if !strings.HasPrefix(name, "hotbed-") {
		return false
	}
	if name == "hotbed-worker" || strings.HasPrefix(name, "hotbed-worker-") {
		return false
	}
	if strings.HasPrefix(name, "hotbed-baseimg-") {
		return false
	}
	return true
}

// isWorkspaceVolume reports whether vol is an engine-owned workspace volume
// ("ws/<id>"). Intermediate datasets like "ws" return false.
func isWorkspaceVolume(vol string) bool {
	rest, ok := strings.CutPrefix(vol, "ws/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// parseBaseVolume parses an engine-owned base volume ("bases/<project>/g<N>").
// Intermediate datasets like "bases" or "bases/<project>" return ok=false.
func parseBaseVolume(vol string) (project string, gen int, ok bool) {
	rest, has := strings.CutPrefix(vol, "bases/")
	if !has {
		return "", 0, false
	}
	proj, gPart, has := strings.Cut(rest, "/")
	if !has || proj == "" || strings.Contains(gPart, "/") {
		return "", 0, false
	}
	numStr, has := strings.CutPrefix(gPart, "g")
	if !has || numStr == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 0 {
		return "", 0, false
	}
	return proj, n, true
}
