package flame

import (
	"cmp"
	"slices"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// StackKey is a folded stack as a map key: its frames (AppendID's key of
// their indexes, root first), its leaf's frame type and its process name.
type StackKey struct {
	Frames   string
	LeafType string
	Comm     string
}

// Stacks counts samples by stack.
type Stacks map[StackKey]int64

// Folded is the stacks as messages naming the frames of the table their
// keys were built from, most sampled first (ties by frames, then process).
func (s Stacks) Folded() []*pb.FoldedStack {
	out := make([]*pb.FoldedStack, 0, len(s))
	for k, n := range s {
		out = append(out, &pb.FoldedStack{Frames: IDs(k.Frames), Samples: n, LeafType: k.LeafType, Comm: k.Comm})
	}
	slices.SortFunc(out, func(a, b *pb.FoldedStack) int {
		return cmp.Or(cmp.Compare(b.Samples, a.Samples), slices.Compare(a.Frames, b.Frames), cmp.Compare(a.Comm, b.Comm))
	})
	return out
}

// Functions counts, for each function samples were in, the samples it was
// in (its total, a recursive function's counted once a stack) and those it
// was the leaf of (its self). The zero Functions is empty and ready to use.
type Functions struct {
	counts map[uint32]*funcCount
}

type funcCount struct{ self, total int64 }

// Add counts n samples of the stack of frame indexes ids, root first.
func (f *Functions) Add(ids []uint32, n int64) {
	if f.counts == nil {
		f.counts = map[uint32]*funcCount{}
	}
	for i, id := range ids {
		c := f.counts[id]
		if c == nil {
			c = &funcCount{}
			f.counts[id] = c
		}
		if !slices.Contains(ids[:i], id) {
			c.total += n
		}
		if i == len(ids)-1 {
			c.self += n
		}
	}
}

// Top is the functions counted, as shares of samples (none when it is 0),
// most self samples first, then most total, then by name in frames (the
// table the indexes name) and index.
func (f *Functions) Top(frames []*pb.ProfileFrame, samples int64) []*pb.ProfileFunction {
	out := make([]*pb.ProfileFunction, 0, len(f.counts))
	for id, c := range f.counts {
		fn := &pb.ProfileFunction{Frame: id, SelfSamples: c.self, TotalSamples: c.total}
		if samples > 0 {
			fn.SelfPct = 100 * float64(c.self) / float64(samples)
			fn.TotalPct = 100 * float64(c.total) / float64(samples)
		}
		out = append(out, fn)
	}
	slices.SortFunc(out, func(a, b *pb.ProfileFunction) int {
		return cmp.Or(
			cmp.Compare(b.SelfSamples, a.SelfSamples),
			cmp.Compare(b.TotalSamples, a.TotalSamples),
			cmp.Compare(frames[a.Frame].Name, frames[b.Frame].Name),
			cmp.Compare(a.Frame, b.Frame),
		)
	})
	return out
}
