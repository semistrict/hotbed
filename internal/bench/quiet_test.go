//go:build !benchtest

package bench

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// clock is a test's time, moved by hand.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

const workDir = "sys/fs/cgroup/user.slice/user-1000.slice/user-1000-hotbed.slice"

// ubu1's: a scratch disk on zd0, which systemd caps once On asks it to,
// and the node's volumes on the ZFS pool rpool.
type quietHost struct {
	*host
	clock *clock
	syncs []string
	iso   *Isolation
}

func newQuietHost(t *testing.T, cfg Config, pool string) *quietHost {
	h := &quietHost{host: newHost(t, "0-10"), clock: &clock{now: time.Unix(1795400000, 0)}}
	h.kernelPartitions("7-10")
	h.write("sys/dev/block/230:0/uevent", "MAJOR=230\nMINOR=0\nDEVNAME=zd0\nDEVTYPE=disk\n")
	h.write(workDir+"/io.max", "")
	h.write(workDir+"/io.stat", "")
	h.iso = h.isolationWith(t, cfg, filepath.Join(t.TempDir(), "bench"), func(o *Options) {
		o.Pool, o.Now = pool, h.clock.Now
		o.SyncPool = func(_ context.Context, pool string) error {
			h.syncs = append(h.syncs, pool)
			return nil
		}
	})
	return h
}

// on isolates as the kernel would: systemd's caps land in the work slice's
// io.max.
func (h *quietHost) on() {
	h.t.Helper()
	if err := h.iso.On(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
	h.write(workDir+"/io.max", fmt.Sprintf("230:0 rbps=64000000 wbps=%d riops=2000 wiops=1000\n", h.iso.writeBound()))
}

// wrote sets what work has written to zd0, and what each dataset has been
// written, by then; then a second passes and the isolation samples.
func (h *quietHost) wrote(zd0 int64, datasets map[string]int64) {
	h.t.Helper()
	h.write(workDir+"/io.stat", fmt.Sprintf("259:0 rbytes=5 wbytes=999999999999 rios=1 wios=1 dbytes=0 dios=0\n230:0 rbytes=100 wbytes=%d rios=1 wios=1 dbytes=0 dios=0\n", zd0))
	n := 0
	for _, ds := range sortedKeys(datasets) {
		n++
		h.write(fmt.Sprintf("proc/spl/kstat/zfs/rpool/objset-0x%x", n), fmt.Sprintf(
			"213 1 0x01 28 7872 7048079248 130705216995423\nname                            type data\ndataset_name                    7    %s\nwrites                          4    18\nnwritten                        4    %d\nreads                           4    8958\n", ds, datasets[ds]))
	}
	h.clock.now = h.clock.now.Add(time.Second)
	h.iso.Sample()
}

// Work writing to the scratch disk past the bound beside a measurement
// (a cap that didn't hold) fails it, naming the disk and the amount; a
// burst a sample long doesn't, as the bound holds over checkWindow (board
// 6887's ubu1 run: 76 MB in 0.3s from a test that started mid-measurement).
func TestHeldFailsOnOthersWritesToADisk(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, "")
	h.on()
	h.wrote(0, nil)
	h.wrote(76_000_000, nil)
	if err := h.iso.Held("ws/m"); err != nil {
		t.Fatalf("Held after a 76 MB burst, under the bound over %v: %v", checkWindow, err)
	}
	for s := range 30 {
		h.wrote(76_000_000+int64(s+1)*9_000_000, nil) // 9 MB/s, past the bound for good
	}
	want := "others wrote 270 MB to zd0 in 30s, past the bound of 8 MB/s over 30s"
	if err := h.iso.Held("ws/m"); err == nil || err.Error() != want {
		t.Errorf("Held = %v, want %q", err, want)
	}
}

// Writes before On aren't the measurement's to answer for.
func TestHeldCountsOnlyWritesSinceOn(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"}, "rpool/hotbed")
	h.write(workDir+"/io.max", "230:0 rbps=max wbps=max riops=max wiops=max\n")
	h.wrote(0, map[string]int64{"rpool/hotbed/ws/b": 0})
	h.wrote(500_000_000, map[string]int64{"rpool/hotbed/ws/b": 500_000_000})
	h.on()
	h.wrote(500_000_000, map[string]int64{"rpool/hotbed/ws/b": 500_000_000})
	h.wrote(500_000_000, map[string]int64{"rpool/hotbed/ws/b": 500_000_000})
	if err := h.iso.Held("ws/m"); err != nil {
		t.Errorf("Held after writes before On only: %v", err)
	}
}

// On a ZFS pool: others' writes to the pool's other volumes past the bound
// fail a measurement, naming the pool, the rate and the heaviest writer;
// the measurement's own volume, and datasets outside the node's, don't
// count. On syncs the pool first.
func TestHeldFailsOnOthersWritesToThePool(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10"}, "rpool/hotbed")
	ds := map[string]int64{"rpool/hotbed/ws/m": 0, "rpool/hotbed/ws/b": 0, "rpool/hotbed/bases/app/g2": 0, "rpool/USERDATA/home": 0}
	h.wrote(0, ds)
	h.on()
	if !slices.Equal(h.syncs, []string{"rpool"}) {
		t.Errorf("pools synced %q, want rpool", h.syncs)
	}
	for range 31 {
		ds["rpool/hotbed/ws/m"] += 900_000_000
		ds["rpool/USERDATA/home"] += 900_000_000
		ds["rpool/hotbed/bases/app/g2"] += 1_000_000
		h.wrote(0, ds)
	}
	if err := h.iso.Held("ws/m"); err != nil {
		t.Fatalf("Held with only its own and others' outside the node's volumes busy: %v", err)
	}
	ds["rpool/hotbed/ws/b"] += 300_000_000
	h.wrote(0, ds)
	want := "others wrote 329 MB to rpool/hotbed (ws/b the most, 300 MB) in 30s, past the bound of 8 MB/s over 30s"
	if err := h.iso.Held("ws/m"); err == nil || err.Error() != want {
		t.Errorf("Held = %v, want %q", err, want)
	}
}

// Loud names the volumes to hold back, heaviest first, until the rest
// write under half the bound, from the last window: before a measurement
// starts too.
func TestLoudNamesTheHeaviestWriters(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10", WorkWriteMB: 10}, "rpool/hotbed")
	ds := map[string]int64{"rpool/hotbed/ws/m": 0, "rpool/hotbed/ws/a": 0, "rpool/hotbed/ws/b": 0, "rpool/hotbed/ws/c": 0}
	h.wrote(0, ds)
	if got := h.iso.Loud("ws/m"); got != nil {
		t.Errorf("Loud from one sample = %v, want none", got)
	}
	for range 2 {
		ds["rpool/hotbed/ws/m"] += 500_000_000
		ds["rpool/hotbed/ws/a"] += 3_000_000
		ds["rpool/hotbed/ws/b"] += 40_000_000
		ds["rpool/hotbed/ws/c"] += 4_000_000
		h.wrote(0, ds)
	}
	want := []Writer{{"ws/b", 40_000_000}, {"ws/c", 4_000_000}}
	if got := h.iso.Loud("ws/m"); !slices.Equal(got, want) {
		t.Errorf("Loud = %v, want %v: a's 3 MB/s is under half the bound", got, want)
	}
	// Once the window has passed them by, they're quiet.
	for range 6 {
		h.wrote(0, ds)
	}
	if got := h.iso.Loud("ws/m"); got != nil {
		t.Errorf("Loud after a quiet window = %v, want none", got)
	}
}

// A btrfs store's disk is capped as the scratch disk is.
func TestOnCapsTheStoresDisk(t *testing.T) {
	h := newHost(t, "0-10")
	h.kernelPartitions("7-10")
	iso := h.isolationWith(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch", WorkWriteMB: 5}, filepath.Join(t.TempDir(), "bench"), func(o *Options) {
		o.StoreDisk = "/var/lib/hotbed/worker/volumes"
	})
	if err := iso.On(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "set user-1000-hotbed.slice" +
		" IOReadBandwidthMax=/mnt/fxdax-scratch 64M IOReadBandwidthMax=/var/lib/hotbed/worker/volumes 64M" +
		" IOWriteBandwidthMax=/mnt/fxdax-scratch 5000000 IOWriteBandwidthMax=/var/lib/hotbed/worker/volumes 5000000" +
		" IOReadIOPSMax=/mnt/fxdax-scratch 2000 IOReadIOPSMax=/var/lib/hotbed/worker/volumes 2000" +
		" IOWriteIOPSMax=/mnt/fxdax-scratch 1000 IOWriteIOPSMax=/var/lib/hotbed/worker/volumes 1000"
	if got := h.sd.calls[len(h.sd.calls)-1]; got != want {
		t.Errorf("systemd's last call\n got %q\nwant %q", got, want)
	}
}

// A measurement whose quiet can't be told isn't held.
func TestHeldFailsWhenWritesCantBeRead(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10"}, "tank/hotbed")
	h.on()
	h.wrote(0, nil)
	if err := h.iso.Held("ws/m"); err == nil {
		t.Error("Held with no kstats for tank: nil, want an error")
	}
}
