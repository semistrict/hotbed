//go:build !benchtest

package bench

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// clock is a test's time, moved by hand.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

const workDir = "sys/fs/cgroup/user.slice/user-1000.slice/user-1000-hotbed.slice"

// A host with a scratch disk on zd0, which systemd caps once On asks it to,
// and optionally VMs whose writes Options.Writes reports.
type quietHost struct {
	*host
	clock *clock
	iso   *Isolation
	// vms is what each volume's VM has written, as Options.Writes says it
	// on a node with VMs (newVMQuietHost), and asks how often it was asked.
	vms  map[string]int64
	asks int
}

func newQuietHost(t *testing.T, cfg Config) *quietHost {
	return newQuietHostWith(t, cfg, nil)
}

// newVMQuietHost is a quietHost whose commands run in VMs, whose writes
// Options.Writes says.
func newVMQuietHost(t *testing.T, cfg Config) *quietHost {
	var h *quietHost
	h = newQuietHostWith(t, cfg, func(o *Options) {
		o.Writes = func() (map[string]int64, error) {
			h.asks++
			return maps.Clone(h.vms), nil
		}
	})
	h.vms = map[string]int64{}
	return h
}

func newQuietHostWith(t *testing.T, cfg Config, more func(*Options)) *quietHost {
	h := &quietHost{host: newHost(t, "0-10"), clock: &clock{now: time.Unix(1795400000, 0)}}
	h.kernelPartitions("7-10")
	h.write("sys/dev/block/230:0/uevent", "MAJOR=230\nMINOR=0\nDEVNAME=zd0\nDEVTYPE=disk\n")
	h.write(workDir+"/io.max", "")
	h.write(workDir+"/io.stat", "")
	h.iso = h.isolationWith(t, cfg, filepath.Join(t.TempDir(), "bench"), func(o *Options) {
		o.Now = h.clock.Now
		if more != nil {
			more(o)
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

// wrote sets what work has written to zd0 by then; then a second passes and
// the isolation samples.
func (h *quietHost) wrote(zd0 int64) {
	h.t.Helper()
	h.write(workDir+"/io.stat", fmt.Sprintf("259:0 rbytes=5 wbytes=999999999999 rios=1 wios=1 dbytes=0 dios=0\n230:0 rbytes=100 wbytes=%d rios=1 wios=1 dbytes=0 dios=0\n", zd0))
	h.clock.now = h.clock.now.Add(time.Second)
	h.iso.Sample(false)
}

// measuring is a second's sample while a measurement waits or runs, its
// VMs having written vms by then (added to what they had).
func (h *quietHost) measuring(vms map[string]int64) {
	h.t.Helper()
	for vol, n := range vms {
		h.vms[vol] += n
	}
	h.clock.now = h.clock.now.Add(time.Second)
	h.iso.Sample(true)
}

// Work writing to the scratch disk past the bound beside a measurement
// (a cap that didn't hold) fails it, naming the disk and the amount; a
// burst a sample long doesn't, as the bound holds over checkWindow (board
// 6887's ubu1 run: 76 MB in 0.3s from a test that started mid-measurement).
func TestHeldFailsOnOthersWritesToADisk(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"})
	h.on()
	h.wrote(0)
	h.wrote(76_000_000)
	if err := h.iso.Held("ws/m"); err != nil {
		t.Fatalf("Held after a 76 MB burst, under the bound over %v: %v", checkWindow, err)
	}
	for s := range 30 {
		h.wrote(76_000_000 + int64(s+1)*9_000_000) // 9 MB/s, past the bound for good
	}
	want := "others wrote 270 MB to zd0 in 30s, past the bound of 8 MB/s over 30s"
	if err := h.iso.Held("ws/m"); err == nil || err.Error() != want {
		t.Errorf("Held = %v, want %q", err, want)
	}
}

// Writes before On aren't the measurement's to answer for.
func TestHeldCountsOnlyWritesSinceOn(t *testing.T) {
	h := newQuietHost(t, Config{CPUs: "7-10", Disk: "/mnt/fxdax-scratch"})
	h.write(workDir+"/io.max", "230:0 rbps=max wbps=max riops=max wiops=max\n")
	h.wrote(0)
	h.wrote(500_000_000)
	h.on()
	h.wrote(500_000_000)
	h.wrote(500_000_000)
	if err := h.iso.Held("ws/m"); err != nil {
		t.Errorf("Held after writes before On only: %v", err)
	}
}

// The VMs' writes, which cladefsd makes for them, past the bound beside a
// measurement fail it, naming the VMs' disks and the heaviest writer; its
// own VM's don't count. What a VM wrote before its writes were first asked
// (as a measurement came to wait) isn't counted against it.
func TestHeldFailsOnOthersVMWrites(t *testing.T) {
	h := newVMQuietHost(t, Config{CPUs: "7-10"})
	if got := h.iso.Quieted(); got != "the VMs' disks" {
		t.Errorf("Quieted = %q, want the VMs' disks", got)
	}
	h.vms["ws/b"] = 50_000_000_000 // since its VM booted
	h.wrote(0)
	if h.asks != 0 {
		t.Errorf("asked the VMs %d times with no measurement, want none", h.asks)
	}
	h.on()
	for range 31 {
		h.measuring(map[string]int64{"ws/m": 900_000_000, "ws/b": 1_000_000})
	}
	if err := h.iso.Held("ws/m"); err != nil {
		t.Fatalf("Held with only its own VM busy: %v", err)
	}
	h.measuring(map[string]int64{"ws/b": 300_000_000})
	want := "others wrote 329 MB to the VMs' disks (ws/b the most, 329 MB) in 30s, past the bound of 8 MB/s over 30s"
	if err := h.iso.Held("ws/m"); err == nil || err.Error() != want {
		t.Errorf("Held = %v, want %q", err, want)
	}
}

// A measurement waits until two samples name the VMs writing (Ready), so
// it holds them before it starts; Loud names them, where they write.
func TestLoudNamesTheHeaviestVMWriters(t *testing.T) {
	h := newVMQuietHost(t, Config{CPUs: "7-10", WorkWriteMB: 10})
	if got := h.iso.Quieted(); got != "the VMs' disks" {
		t.Errorf("Quieted = %q, want the VMs' disks", got)
	}
	h.wrote(0)
	h.wrote(0)
	if h.iso.Ready() {
		t.Error("Ready before the VMs were asked: true, want false")
	}
	h.measuring(nil)
	if h.iso.Ready() {
		t.Error("Ready after one sample of the VMs: true, want false")
	}
	h.measuring(map[string]int64{"ws/m": 500_000_000, "ws/b": 40_000_000, "ws/c": 3_000_000})
	if !h.iso.Ready() {
		t.Error("Ready after two samples of the VMs: false, want true")
	}
	want := []Writer{{Volume: "ws/b", Rate: 40_000_000, To: "the VMs' disks"}}
	if got := h.iso.Loud("ws/m"); !slices.Equal(got, want) {
		t.Errorf("Loud = %v, want %v: c's 3 MB/s is under half the bound", got, want)
	}
	// Unasked a while (no measurement waits), it is not Ready again.
	for range 4 {
		h.wrote(0)
	}
	if h.iso.Ready() {
		t.Error("Ready after the VMs went unasked past the window: true, want false")
	}
}

// A measurement whose VMs' writes can't be read isn't kept waiting for
// samples that never come: it starts, and Held fails it saying why.
func TestReadyWhenTheVMsCantBeAsked(t *testing.T) {
	h := newVMQuietHost(t, Config{CPUs: "7-10"})
	h.iso.o.Writes = func() (map[string]int64, error) { return nil, errors.New("cladefsd: connection refused") }
	h.measuring(nil)
	if !h.iso.Ready() {
		t.Fatal("Ready with the VMs' writes unreadable: false, want true")
	}
	h.on()
	h.measuring(nil)
	want := "what others write beside it can't be read: cladefsd: connection refused"
	if err := h.iso.Held("ws/m"); err == nil || err.Error() != want {
		t.Errorf("Held = %v, want %q", err, want)
	}
}
