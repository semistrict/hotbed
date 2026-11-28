package bench

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Quiet disks. Others' writes to a disk a measurement uses slow it down,
// but they don't fail it, so they skew its numbers without saying so. While
// one runs, other work may write at most the bound (Config.WorkWriteMB) to
// each disk the measurement uses:
//
//   - A block device (the scratch disk): the work slice's io.max caps it at
//     the bound, which holds where the filesystem charges writeback to the
//     writer's cgroup (ext4, xfs). The work slice's io.stat says what it wrote.
//   - The VMs' disks: cladefsd writes them for every VM, from threads of
//     its own, so no cap reaches a VM's writes. Each VM's guest tells
//     cladefsd what it wrote (Options.Writes), and the worker holds back
//     (parks) the loud ones' commands before the measurement starts and
//     while it runs. Asking costs every VM a little, so Sample asks only
//     while a measurement waits or runs, and one starts once two samples
//     name the writers to hold (Ready).
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
	// on, by device name (nvme0n1).
	disks map[string]int64
	// vms is what each volume's VM wrote to its disk (Options.Writes); nil
	// when not asked.
	vms map[string]int64
	err error
}

// Writer is a volume writing to a place a measurement uses (the VMs'
// disks), and how fast, in bytes a second.
type Writer struct {
	Volume string
	Rate   float64
	To     string
}

// place is somewhere the worker keeps quiet by holding writers back: its
// name, for people, and what a sample says was written to it, by volume
// (nil where the sample didn't ask).
type place struct {
	name string
	vols func(written) map[string]int64
	// costly is a place Sample asks only while a measurement waits or
	// runs.
	costly bool
}

// places is where the worker holds writers back: the VMs' disks.
func (i *Isolation) places() []place {
	if i.o.Writes == nil {
		return nil
	}
	return []place{{name: i.writesTo(), vols: func(w written) map[string]int64 { return w.vms }, costly: true}}
}

func (i *Isolation) writesTo() string {
	if i.o.WritesTo == "" {
		return "the VMs' disks"
	}
	return i.o.WritesTo
}

// Quieted is where the worker holds back writers beside a measurement
// (Loud), which no cap reaches, or "" for nowhere.
func (i *Isolation) Quieted() string {
	var names []string
	for _, p := range i.places() {
		names = append(names, p.name)
	}
	return strings.Join(names, " and ")
}

// sampled is the samples of ws that asked p.
func (p place) sampled(ws []written) []written {
	var out []written
	for _, w := range ws {
		if p.vols(w) != nil {
			out = append(out, w)
		}
	}
	return out
}

// Ready says whether a measurement may start as far as its quiet goes:
// whether the samples name who writes to every place it holds writers back
// on, so it can hold them first (Loud). One whose writers can't be read
// starts, to fail at once saying why (Held), rather than wait for good.
func (i *Isolation) Ready() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if n := len(i.written); n > 0 && i.written[n-1].err != nil {
		return true
	}
	ws := i.recent(time.Time{}, holdWindow)
	for _, p := range i.places() {
		if len(p.sampled(ws)) < 2 {
			return false
		}
	}
	return true
}

// Sample reads what others have written so far, for Held and Loud: the
// worker calls it once a step, whether or not a measurement runs, so that
// one that starts finds the last seconds' writers. measuring says whether
// a measurement waits or runs, without which the costly places go unasked.
func (i *Isolation) Sample(measuring bool) {
	w := written{at: i.o.Now()}
	w.disks, w.err = i.diskWrites()
	if w.err == nil && i.o.Writes != nil && measuring {
		w.vms, w.err = i.o.Writes()
		if w.err == nil && w.vms == nil {
			w.vms = map[string]int64{} // asked: no VM wrote
		}
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
	if len(ws) >= 2 {
		first, last := ws[0], ws[len(ws)-1]
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
			if err := i.over(grew(from.disks, last.disks, name), name, first, last); err != nil {
				return err
			}
		}
	}
	for _, p := range i.places() {
		ps := p.sampled(ws)
		if len(ps) < 2 {
			continue
		}
		first, last := ps[0], ps[len(ps)-1]
		if vols, total := grown(p.vols(first), p.vols(last), own); total > 0 {
			where := fmt.Sprintf("%s (%s the most, %.0f MB)", p.name, vols[0].vol, float64(vols[0].n)/1e6)
			if err := i.over(total, where, first, last); err != nil {
				return err
			}
		}
	}
	return nil
}

// over says how n bytes others wrote to where from sample first to last
// went past the bound, or nil if they didn't: the bound over checkWindow,
// or the span if longer, and a second's more, as a kernel's cap lets a
// little through ahead of time.
func (i *Isolation) over(n int64, where string, first, last written) error {
	span := last.at.Sub(first.at)
	judged := max(span, checkWindow)
	if float64(n) <= float64(i.writeBound())*(judged+time.Second).Seconds() {
		return nil
	}
	return fmt.Errorf("others wrote %.0f MB to %s in %s, past the bound of %d MB/s over %s",
		float64(n)/1e6, where, span.Round(100*time.Millisecond), i.writeBound()/1_000_000, judged.Round(time.Second))
}

// Loud is the volumes, other than own (the measurement's), that the worker
// must hold back for the others' writes to each place (the pool, the VMs'
// disks) to stay under half the bound, as the last holdWindow saw them:
// the heaviest first, place by place. Half leaves room for writers that
// start meanwhile, until a sample sees them.
func (i *Isolation) Loud(own string) []Writer {
	i.mu.Lock()
	defer i.mu.Unlock()
	ws := i.recent(time.Time{}, holdWindow)
	var loud []Writer
	for _, p := range i.places() {
		ps := p.sampled(ws)
		if len(ps) < 2 {
			continue
		}
		first, last := ps[0], ps[len(ps)-1]
		span := last.at.Sub(first.at).Seconds()
		vols, total := grown(p.vols(first), p.vols(last), own)
		for _, v := range vols {
			if float64(total)/span <= float64(i.writeBound())/2 {
				break
			}
			total -= v.n
			loud = append(loud, Writer{Volume: v.vol, Rate: float64(v.n) / span, To: p.name})
		}
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
