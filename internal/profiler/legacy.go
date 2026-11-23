package profiler

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Admit makes a profile from elsewhere (stored, in a worker's event or its
// reply) one this build can read, or says why it cannot be: one from before
// frames had a table comes in today's shape, and every frame index must name
// a frame. Old profiles outlive the build that wrote them: in workers' event
// logs, in stored rows and files, and from workers still on an older build
// while they follow the controller's.
func Admit(p *pb.CommandProfile) error {
	if _, err := UpgradeLegacyProfile(p); err != nil {
		return err
	}
	if err := flame.Check(p.Frames, p.Stacks, p.TopFunctions); err != nil {
		return fmt.Errorf("profile %s: %w", p.CommandId, err)
	}
	nFrames := uint32(len(p.Frames))
	nStacks := uint32(len(p.Stacks))
	for i, b := range p.Buckets {
		if b.TopFrame != noTopFrame && b.TopFrame >= nFrames {
			return fmt.Errorf("profile %s: bucket %d names frame %d of %d", p.CommandId, i, b.TopFrame, nFrames)
		}
		if len(b.StackIndexes) != len(b.StackSamples) {
			return fmt.Errorf("profile %s: bucket %d has %d stack indexes and %d sample counts", p.CommandId, i, len(b.StackIndexes), len(b.StackSamples))
		}
		for j, idx := range b.StackIndexes {
			if idx >= nStacks {
				return fmt.Errorf("profile %s: bucket %d stack %d names stack %d of %d", p.CommandId, i, j, idx, nStacks)
			}
			if b.StackSamples[j] < 0 {
				return fmt.Errorf("profile %s: bucket %d stack %d has negative samples %d", p.CommandId, i, j, b.StackSamples[j])
			}
		}
		if b.DurationMs == 0 {
			// Buckets were all 5 seconds long before they said how long.
			b.DurationMs = bucketDurationMs
		}
	}
	return nil
}

// UpgradeLegacyProfile rewrites a CommandProfile from before frames had a
// table of their own into today's shape, and says whether it was one. Such
// a profile decodes into today's message with its old fields kept as unknown
// ones: a stack's frames as one string of folded labels (FoldedStack field
// 1), a function's frame as its own fields (ProfileFunction fields 1 to 5),
// and a bucket's top frame as a label (ProfileBucket field 5).
func UpgradeLegacyProfile(p *pb.CommandProfile) (bool, error) {
	legacy := false
	for _, st := range p.Stacks {
		legacy = legacy || len(st.ProtoReflect().GetUnknown()) > 0
	}
	for _, fn := range p.TopFunctions {
		legacy = legacy || len(fn.ProtoReflect().GetUnknown()) > 0
	}
	for _, b := range p.Buckets {
		legacy = legacy || len(b.ProtoReflect().GetUnknown()) > 0
	}
	if !legacy {
		return false, nil
	}

	// Old functions carried their frame's metadata; old stacks and buckets
	// only the frame's label.
	type oldFunction struct {
		fn    *pb.ProfileFunction
		frame *pb.ProfileFrame
	}
	known := map[string]*pb.ProfileFrame{}
	fns := make([]oldFunction, 0, len(p.TopFunctions))
	for _, fn := range p.TopFunctions {
		fr := &pb.ProfileFrame{}
		err := eachUnknown(fn.ProtoReflect().GetUnknown(), func(num protowire.Number, v []byte, n uint64) {
			switch num {
			case 1:
				fr.Name = string(v)
			case 2:
				fr.Module = string(v)
			case 3:
				fr.FrameType = string(v)
			case 4:
				fr.SourceFile = string(v)
			case 5:
				fr.SourceLine = int32(n)
			}
		})
		if err != nil {
			return false, fmt.Errorf("profile %s: function: %w", p.CommandId, err)
		}
		if fr.FrameType == "kernel" {
			fr.Name = strings.TrimSuffix(fr.Name, "_[k]")
		}
		fns = append(fns, oldFunction{fn, fr})
		known[flame.Label(fr)] = fr
	}

	// A label names one frame, as it did in the store that wrote it: the
	// function's, if it was a top one, or else what the label shows of it,
	// as first seen.
	var t flame.Table
	ids := map[string]uint32{}
	id := func(label, leafType string, leaf bool) uint32 {
		if id, ok := ids[label]; ok {
			return id
		}
		fr, ok := known[label]
		if !ok {
			fr = &pb.ProfileFrame{Name: label, FrameType: "native"}
			if name, ok := strings.CutSuffix(label, "_[k]"); ok {
				fr.Name, fr.FrameType = name, "kernel"
			} else if leaf && leafType != "" {
				fr.FrameType = leafType
			}
		}
		ids[label] = t.ID(fr)
		return ids[label]
	}
	for _, st := range p.Stacks {
		var folded string
		err := eachUnknown(st.ProtoReflect().GetUnknown(), func(num protowire.Number, v []byte, _ uint64) {
			if num == 1 {
				folded = string(v)
			}
		})
		if err != nil {
			return false, fmt.Errorf("profile %s: stack: %w", p.CommandId, err)
		}
		labels := strings.Split(folded, ";")
		st.Frames = st.Frames[:0]
		for i, lbl := range labels {
			if lbl != "" {
				st.Frames = append(st.Frames, id(lbl, st.LeafType, i == len(labels)-1))
			}
		}
		st.ProtoReflect().SetUnknown(nil)
	}
	for _, f := range fns {
		f.fn.Frame = id(flame.Label(f.frame), "", false)
		f.fn.ProtoReflect().SetUnknown(nil)
	}
	for _, b := range p.Buckets {
		var top string
		err := eachUnknown(b.ProtoReflect().GetUnknown(), func(num protowire.Number, v []byte, _ uint64) {
			if num == 5 {
				top = string(v)
			}
		})
		if err != nil {
			return false, fmt.Errorf("profile %s: bucket: %w", p.CommandId, err)
		}
		if top == "" {
			b.TopFrame = noTopFrame
		} else {
			b.TopFrame = id(top, "", false)
		}
		b.ProtoReflect().SetUnknown(nil)
	}
	p.Frames = t.Frames()
	return true, nil
}

// eachUnknown calls f with each field in raw, encoded unknown fields: v for
// a length-delimited one, n for a varint.
func eachUnknown(raw []byte, f func(num protowire.Number, v []byte, n uint64)) error {
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return protowire.ParseError(n)
		}
		raw = raw[n:]
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			f(num, v, 0)
			n = m
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			f(num, nil, v)
			n = m
		default:
			n = protowire.ConsumeFieldValue(num, typ, raw)
			if n < 0 {
				return protowire.ParseError(n)
			}
		}
		raw = raw[n:]
	}
	return nil
}
