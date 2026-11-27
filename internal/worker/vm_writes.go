package worker

import (
	"context"
	"errors"
	"fmt"
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

// cellKey is a cell, by tenant and number.
type cellKey struct{ tenant, cell uint64 }

// VolumeWrites is what each VM has written to its disk since it booted, by
// the volume of its workspace (ws/3f2a01), as its guest tells cladefsd:
// cladefsd writes the VMs' disks for them, from threads no cgroup's IO cap
// nor any ZFS dataset's kstat ties to a workspace (bench.Options.Writes). A
// VM of no workspace (a base's build) is named by its own ID, one the
// worker doesn't know by its cell. A VM that doesn't answer in time counts
// what it said last.
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
	vols := b.cellVolumes()
	b.writesMu.Lock()
	defer b.writesMu.Unlock()
	last := b.lastWrites
	b.lastWrites = map[uint64]uint64{}
	out := map[string]int64{}
	for _, v := range stats.GetVms() {
		n, ok := v.GetGuest().GetCounters()[guestWrittenCounter]
		if !v.GetAnswered() || !ok {
			if n, ok = last[v.GetConnection()]; !ok {
				continue // never heard from: nothing to count from yet
			}
		}
		b.lastWrites[v.GetConnection()] = n
		vol, known := vols[cellKey{v.GetTenant(), v.GetCell()}]
		if !known {
			vol = fmt.Sprintf("cell %d/%d", v.GetTenant(), v.GetCell())
		}
		out[vol] += int64(n)
	}
	return out, nil
}

// cellVolumes is the volume each cell a VM of the backend runs on is
// written for: a workspace's, or a base build's ID.
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
