package bench

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Quiet disks. Others' writes to a disk a measurement uses slow it down,
// but they don't fail it, so they skew its numbers without saying so. While
// one runs, other work may write at most the bound (Config.WorkWriteMB) to
// each disk or pool the measurement uses:
//
//   - A block device (the scratch disk, a btrfs store's disk): the work
//     slice's io.max caps it at the bound, which holds where the filesystem
//     charges writeback to the writer's cgroup (ext4, btrfs). The work
//     slice's io.stat says what it wrote.
//   - A ZFS pool: ZFS writes a pool from threads of its own, charged to no
//     one's cgroup, so no cap reaches its writers (a unit capped at 8 MB/s
//     on ubu1's pool vdev wrote 147 MB/s to it). Each dataset's kstat says
//     what was written to it, so the worker holds back (parks) the commands
//     of the volumes Loud names, before the measurement starts and while it
//     runs.
//
// Either way Held fails a measurement whose disks saw others write past the
// bound anyway, averaged over checkWindow, as a caller's own contamination
// check averages over its rounds: a writer that starts mid-measurement
// writes for a sample's time before it is held, and that burst alone
// doesn't fail it.

const (
	// holdWindow is the span over which Loud finds writers to hold: short,
	// so one is held a sample or two after it starts.
	holdWindow = 3 * time.Second
	// checkWindow is the span Held averages others' writes over, at least:
	// a measurement younger than it is judged as though it were that old.
	checkWindow = 30 * time.Second
)

// written is what others had written by one Sample, in bytes.
type written struct {
	at time.Time
	// disks is the work slice's writes to each block device it is capped
	// on, by device name (zd0).
	disks map[string]int64
	// vols is what was written to each volume of the pool (ws/3f2a01).
	vols map[string]int64
	err  error
}

// Writer is a volume writing to the pool a measurement uses, and how fast,
// in bytes a second.
type Writer struct {
	Volume string
	Rate   float64
}

// Sample reads what others have written so far, for Held and Loud: the
// worker calls it once a step, whether or not a measurement runs, so that
// one that starts finds the last seconds' writers.
func (i *Isolation) Sample() {
	w := written{at: i.o.Now()}
	w.disks, w.err = i.diskWrites()
	if w.err == nil && i.o.Pool != "" {
		w.vols, w.err = i.poolWrites()
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.written = append(i.written, w)
	// One older than the window stays, to count the whole window from.
	n := 0
	for n+1 < len(i.written) && i.written[n+1].at.Before(w.at.Add(-checkWindow)) {
		n++
	}
	i.written = slices.Delete(i.written, 0, n)
}

// recent is the samples in the last window, from one at or before its
// start, taken after from. Call with i.mu held.
func (i *Isolation) recent(from time.Time, window time.Duration) []written {
	if len(i.written) == 0 {
		return nil
	}
	last := i.written[len(i.written)-1].at
	var out []written
	for n, w := range i.written {
		if w.at.Before(from) {
			continue
		}
		if n+1 < len(i.written) && !i.written[n+1].at.After(last.Add(-window)) {
			continue // the next one starts the window too
		}
		out = append(out, w)
	}
	return out
}

// quiet says which disk others wrote to past the bound beside the
// measurement in volume own, over the last window since On; nil while none.
func (i *Isolation) quiet(own string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.written) > 0 {
		if err := i.written[len(i.written)-1].err; err != nil {
			return fmt.Errorf("what others write beside it can't be read: %w", err)
		}
	}
	ws := i.recent(i.since, checkWindow)
	if len(ws) < 2 {
		return nil
	}
	first, last := ws[0], ws[len(ws)-1]
	span := last.at.Sub(first.at)
	// The bound over the window, or the span if longer, and a second's
	// more: a kernel's cap lets a little through ahead of time.
	judged := max(span, checkWindow)
	allowed := float64(i.writeBound()) * (judged + time.Second).Seconds()
	over := func(n int64, where string) error {
		return fmt.Errorf("others wrote %.0f MB to %s in %s, past the bound of %d MB/s over %s",
			float64(n)/1e6, where, span.Round(100*time.Millisecond), i.writeBound()/1_000_000, judged.Round(time.Second))
	}
	for _, name := range sortedKeys(last.disks) {
		// A disk capped since the window began counts from its first
		// sample: what it wrote before it was capped is no one's.
		from := first
		for _, w := range ws {
			if _, ok := w.disks[name]; ok {
				from = w
				break
			}
		}
		if n := grew(from.disks, last.disks, name); float64(n) > allowed {
			return over(n, name)
		}
	}
	if vols, total := grown(first.vols, last.vols, own); float64(total) > allowed {
		return over(total, fmt.Sprintf("%s (%s the most, %.0f MB)", i.o.Pool, vols[0].vol, float64(vols[0].n)/1e6))
	}
	return nil
}

// Loud is the volumes, other than own (the measurement's), that the worker
// must hold back for the others' writes to the pool to stay under half the
// bound, as the last holdWindow saw them: the heaviest first. Half leaves
// room for writers that start meanwhile, until a sample sees them.
func (i *Isolation) Loud(own string) []Writer {
	i.mu.Lock()
	defer i.mu.Unlock()
	ws := i.recent(time.Time{}, holdWindow)
	if len(ws) < 2 {
		return nil
	}
	first, last := ws[0], ws[len(ws)-1]
	span := last.at.Sub(first.at).Seconds()
	vols, total := grown(first.vols, last.vols, own)
	var loud []Writer
	for _, v := range vols {
		if float64(total)/span <= float64(i.writeBound())/2 {
			break
		}
		total -= v.n
		loud = append(loud, Writer{Volume: v.vol, Rate: float64(v.n) / span})
	}
	return loud
}

// grew is how much counter name rose from a to b: a counter that is new,
// or started again (a dataset mounted anew), from nothing.
func grew(a, b map[string]int64, name string) int64 {
	if n, ok := a[name]; ok && n <= b[name] {
		return b[name] - n
	}
	return b[name]
}

// volBytes is the bytes written to a volume.
type volBytes struct {
	vol string
	n   int64
}

// grown is each volume's writes from a to b but own's, heaviest first, and
// their sum.
func grown(a, b map[string]int64, own string) ([]volBytes, int64) {
	var out []volBytes
	var total int64
	for _, v := range sortedKeys(b) {
		if v == own {
			continue
		}
		if n := grew(a, b, v); n > 0 {
			out = append(out, volBytes{v, n})
			total += n
		}
	}
	slices.SortStableFunc(out, func(x, y volBytes) int { return cmp.Compare(y.n, x.n) })
	return out, total
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// diskWrites is what the work slice wrote to each block device its io.max
// caps writes on, by device name.
func (i *Isolation) diskWrites() (map[string]int64, error) {
	dir := sliceDir(i.o.WorkSlice)
	limits, err := i.read(dir + "/io.max")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no IO controller there, nothing capped
		}
		return nil, err
	}
	capped := map[string]bool{}
	for _, l := range strings.Split(limits, "\n") {
		if dev, rest, ok := strings.Cut(l, " "); ok && !strings.Contains(rest, "wbps=max") {
			capped[dev] = true
		}
	}
	if len(capped) == 0 {
		return nil, nil
	}
	stat, err := i.read(dir + "/io.stat")
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for dev := range capped {
		out[i.devName(dev)] = 0
	}
	for _, l := range strings.Split(stat, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || !capped[f[0]] {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "wbytes="); ok {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%s/io.stat: %q: %w", dir, kv, err)
				}
				out[i.devName(f[0])] = n
			}
		}
	}
	return out, nil
}

// devName is block device major:minor's name (zd0), or major:minor itself
// where the kernel names none.
func (i *Isolation) devName(dev string) string {
	uevent, err := i.read("/sys/dev/block/" + dev + "/uevent")
	if err != nil {
		return dev
	}
	for _, l := range strings.Split(uevent, "\n") {
		if name, ok := strings.CutPrefix(l, "DEVNAME="); ok {
			return name
		}
	}
	return dev
}

// poolWrites is what was written to each volume under o.Pool, by ZFS's
// per-dataset kstats: the bytes write(2) handed each one.
func (i *Isolation) poolWrites() (map[string]int64, error) {
	dir := "/proc/spl/kstat/zfs/" + poolOf(i.o.Pool)
	entries, err := os.ReadDir(i.path(dir))
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "objset-") {
			continue
		}
		ds, n, err := i.objset(dir + "/" + e.Name())
		if os.IsNotExist(err) {
			continue // a dataset unmounted meanwhile
		}
		if err != nil {
			return nil, err
		}
		if vol, ok := strings.CutPrefix(ds, i.o.Pool+"/"); ok {
			out[vol] = n
		}
	}
	return out, nil
}

// objset is a dataset's kstat's name and the bytes written to it.
func (i *Isolation) objset(p string) (string, int64, error) {
	f, err := os.Open(i.path(p))
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	var ds string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		switch fields[0] {
		case "dataset_name":
			ds = fields[2]
		case "nwritten":
			n, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return "", 0, fmt.Errorf("%s: nwritten %q: %w", p, fields[2], err)
			}
			return ds, n, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, err
	}
	return "", 0, fmt.Errorf("%s: no nwritten", p)
}

// poolOf is the pool a dataset is in.
func poolOf(dataset string) string {
	pool, _, _ := strings.Cut(dataset, "/")
	return pool
}

func syncPool(ctx context.Context, pool string) error {
	if out, err := exec.CommandContext(ctx, "zpool", "sync", pool).CombinedOutput(); err != nil {
		return fmt.Errorf("zpool sync %s: %w: %s", pool, err, strings.TrimSpace(string(out)))
	}
	return nil
}
