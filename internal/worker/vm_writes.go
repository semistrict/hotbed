package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"

	"github.com/semistrict/hotbed/internal/cladefs"
	"github.com/semistrict/hotbed/internal/vm"
)

// guestWrittenCounter is the guest counter of what a VM has written to its
// disk, through cladefsd: a 512 MiB dd in a workspace's VM on ubu1 raised it
// by 512 MiB (hotbed board 14487).
const guestWrittenCounter = "device_write_bytes"

// vmWritesWait is how long VolumeWrites waits for the VMs to answer.
const vmWritesWait = 500 * time.Millisecond

// endedWritesKept is how long a volume's VMs that ended are still counted
// after the last of them: longer than any span a measurement's quiet is
// judged over, so a writer whose VM stopped mid-window still counts.
const endedWritesKept = 2 * time.Minute

// cellKey is a cell, by tenant and number.
type cellKey struct{ tenant, cell uint64 }

// vmWrites is what VolumeWrites keeps between calls, so that each volume's
// count only grows: what each VM (by its cladefsd connection) has written
// since it was first heard from, and what VMs that ended wrote.
type vmWrites struct {
	mu sync.Mutex
	// asked says a listing of the VMs was read before: a VM not in it
	// booted since, and counts from nothing.
	asked bool
	// listed is the VMs the last listing named, heard from or not.
	listed map[uint64]*vmWriter
	ended  map[string]endedWrites
}

// vmWriter is one VM's writes: what it said last, and from what it counts.
type vmWriter struct {
	vol string
	// booted says it booted since an earlier listing, so all it wrote is
	// new; else it counts from what it first said (heard), what it wrote
	// before being unknown.
	booted      bool
	heard       bool
	base, wrote uint64
}

func (w *vmWriter) count() uint64 {
	if !w.heard {
		return 0
	}
	return w.wrote - w.base
}

// endedWrites is what a volume's VMs that ended wrote, and when the last of
// them ended.
type endedWrites struct {
	n  uint64
	at time.Time
}

// VolumeWrites is what each VM has written to its disk, by the volume it
// writes for (a workspace's, ws/3f2a01, or a base build's VM ID), as its
// guest tells cladefsd: cladefsd writes the VMs' disks for them, from
// threads no cgroup's IO cap nor any ZFS dataset's kstat ties to a volume
// (bench.Options.Writes). Each volume's count only grows: a VM that ended
// still counts (for endedWritesKept), one that doesn't answer counts what it
// said last, and one first heard from late counts from then, not its whole
// life, unless it booted since the last call.
func (b *VMBackend) VolumeWrites() (map[string]int64, error) {
	store := b.currentStore()
	if store == nil {
		return nil, errors.New("no cladefsd connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), vmWritesWait+time.Second)
	defer cancel()
	stats, err := store.GetStats(ctx, &cladefsdv1.GetStatsRequest{WaitMs: uint32(vmWritesWait.Milliseconds()), LeaveOutHistograms: true})
	if err != nil {
		return nil, fmt.Errorf("ask cladefsd what the VMs wrote: %w", err)
	}
	return b.writes.add(stats.GetVms(), b.cellVolumes(), time.Now()), nil
}

// add takes a listing of the VMs, named by vols, at now, and says what each
// volume has written.
func (s *vmWrites) add(vms []*cladefsdv1.VmStats, vols map[cellKey]string, now time.Time) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended == nil {
		s.ended = map[string]endedWrites{}
	}
	listed := map[uint64]*vmWriter{}
	for _, v := range vms {
		w := s.listed[v.GetConnection()]
		if w == nil {
			vol, ok := vols[cellKey{v.GetTenant(), v.GetCell()}]
			if !ok {
				vol = fmt.Sprintf("cell %d/%d", v.GetTenant(), v.GetCell())
			}
			w = &vmWriter{vol: vol, booted: s.asked}
		}
		if n, ok := v.GetGuest().GetCounters()[guestWrittenCounter]; v.GetAnswered() && ok {
			if !w.heard && !w.booted {
				w.base = n
			}
			w.heard, w.wrote = true, n
		}
		listed[v.GetConnection()] = w
	}
	for conn, w := range s.listed {
		if _, ok := listed[conn]; !ok {
			s.ended[w.vol] = endedWrites{n: s.ended[w.vol].n + w.count(), at: now}
		}
	}
	s.listed, s.asked = listed, true

	out := map[string]int64{}
	for vol, e := range s.ended {
		if now.Sub(e.at) > endedWritesKept {
			delete(s.ended, vol)
			continue
		}
		out[vol] += int64(e.n)
	}
	for _, w := range listed {
		out[w.vol] += int64(w.count())
	}
	return out
}

// cellVolumes is the volume each cell a VM of the backend runs on is
// written for: a workspace's, or a base build's VM ID.
func (b *VMBackend) cellVolumes() map[cellKey]string {
	out := map[cellKey]string{}
	if b.mgr != nil {
		for _, id := range b.mgr.List() {
			inst := b.mgr.Get(id)
			if inst == nil {
				continue
			}
			if t, c, ok := cladefs.ParseCellID(inst.CellPath); ok {
				if vm.IsEphemeralBuildID(id) {
					out[cellKey{t, c}] = id
				} else {
					out[cellKey{t, c}] = wsVolume(id)
				}
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ref := range b.wsCells {
		out[cellKey{ref.Tenant, ref.Cell}] = wsVolume(id)
	}
	return out
}
