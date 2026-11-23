// Package flame holds the shape sampled stacks travel in. A message carrying
// stacks (a command's continuous profile, a trace, a process sample) lists
// each frame once, in its frames table, and names frames everywhere else by
// their index there, since deep stacks of long names (a JVM's, say) would
// otherwise repeat every name in every stack and outgrow any message.
package flame

import (
	"encoding/binary"
	"fmt"
	"strings"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Table builds a message's frames table, listing each function once. A
// function is a name of a frame type: whichever module and source line its
// samples were at, it counts as one, with those of the first seen. The zero
// Table is empty and ready to use.
type Table struct {
	frames []*pb.ProfileFrame
	ids    map[frameKey]uint32
}

type frameKey struct{ name, frameType string }

// Lookup returns the index of the function with this name and frame type,
// if the table has it.
func (t *Table) Lookup(name, frameType string) (uint32, bool) {
	id, ok := t.ids[frameKey{name, frameType}]
	return id, ok
}

// ID returns the index of f's function, adding a copy of f if the table
// lacks it.
func (t *Table) ID(f *pb.ProfileFrame) uint32 {
	k := frameKey{f.GetName(), f.GetFrameType()}
	if id, ok := t.ids[k]; ok {
		return id
	}
	if t.ids == nil {
		t.ids = map[frameKey]uint32{}
	}
	id := uint32(len(t.frames))
	t.ids[k] = id
	t.frames = append(t.frames, &pb.ProfileFrame{
		Name: f.GetName(), Module: f.GetModule(), FrameType: f.GetFrameType(), SourceFile: f.GetSourceFile(), SourceLine: f.GetSourceLine(),
	})
	return id
}

// Frames is the table, for a message's frames field.
func (t *Table) Frames() []*pb.ProfileFrame { return t.frames }

// Rebase rewrites stacks and functions, which name frames by their index in
// from, to name them by their index in t, adding the frames t lacks. It
// changes them in place.
func (t *Table) Rebase(from []*pb.ProfileFrame, stacks []*pb.FoldedStack, fns []*pb.ProfileFunction) {
	for _, st := range stacks {
		for i, id := range st.Frames {
			st.Frames[i] = t.ID(from[id])
		}
	}
	for _, fn := range fns {
		fn.Frame = t.ID(from[fn.Frame])
	}
}

// AppendID appends a frame index to a stack's key: the four bytes of each
// of its frames' indexes, root first, a string a map can be keyed by.
func AppendID(key []byte, id uint32) []byte { return binary.LittleEndian.AppendUint32(key, id) }

// IDs are the frame indexes of a key AppendID built.
func IDs(key string) []uint32 {
	ids := make([]uint32, len(key)/4)
	for i := range ids {
		ids[i] = binary.LittleEndian.Uint32([]byte(key[4*i:]))
	}
	return ids
}

// Check says whether every frame index in stacks, fns and tops (buckets'
// top frames) names a frame in frames: a message from elsewhere may not.
func Check(frames []*pb.ProfileFrame, stacks []*pb.FoldedStack, fns []*pb.ProfileFunction, tops ...uint32) error {
	n := uint32(len(frames))
	for i, st := range stacks {
		for _, id := range st.Frames {
			if id >= n {
				return fmt.Errorf("stack %d names frame %d of %d", i, id, n)
			}
		}
	}
	for i, fn := range fns {
		if fn.Frame >= n {
			return fmt.Errorf("function %d names frame %d of %d", i, fn.Frame, n)
		}
	}
	for i, id := range tops {
		if id >= n {
			return fmt.Errorf("bucket %d names frame %d of %d", i, id, n)
		}
	}
	return nil
}

// Label is a frame as flamegraph.pl's folded format writes it: semicolons,
// which separate frames there, become colons, and kernel frames end in
// "_[k]".
func Label(f *pb.ProfileFrame) string { return LabelOf(f.GetName(), f.GetFrameType()) }

// LabelOf is Label of a frame with this name and type.
func LabelOf(name, frameType string) string {
	name = strings.ReplaceAll(name, ";", ":")
	if name == "" {
		name = "[unknown]"
	}
	if frameType == "kernel" && !strings.HasSuffix(name, "_[k]") {
		name += "_[k]"
	}
	return name
}

// Fold writes a stack the way flamegraph.pl's folded format does, root
// first, without its sample count.
func Fold(frames []*pb.ProfileFrame, st *pb.FoldedStack) string {
	var b strings.Builder
	for i, id := range st.Frames {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(Label(frames[id]))
	}
	return b.String()
}
