// Package profilertest has what tests of stored profiles share.
package profilertest

import "google.golang.org/protobuf/encoding/protowire"

// LegacyProfile encodes a CommandProfile as hotbed wrote them before frames
// had a table: stacks as folded strings, functions with their frame's
// fields, buckets naming their top frame's label. Its command is
// cmd0000001; its stacks fold to "main.main;main.work;vfs_write_[k]" (3
// samples) and "main.main;main.work;runtime.mallocgc" (2).
func LegacyProfile() []byte {
	msg := func(fields ...func([]byte) []byte) []byte {
		var b []byte
		for _, f := range fields {
			b = f(b)
		}
		return b
	}
	str := func(num protowire.Number, v string) func([]byte) []byte {
		return func(b []byte) []byte {
			return protowire.AppendString(protowire.AppendTag(b, num, protowire.BytesType), v)
		}
	}
	varint := func(num protowire.Number, v uint64) func([]byte) []byte {
		return func(b []byte) []byte {
			return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v)
		}
	}
	sub := func(num protowire.Number, fields ...func([]byte) []byte) func([]byte) []byte {
		return func(b []byte) []byte {
			return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), msg(fields...))
		}
	}
	return msg(
		str(1, "cmd0000001"),
		varint(9, 5),
		sub(15, str(1, "main.main;main.work;vfs_write_[k]"), varint(2, 3), str(3, "kernel"), str(4, "app")),
		sub(15, str(1, "main.main;main.work;runtime.mallocgc"), varint(2, 2), str(3, "go"), str(4, "app")),
		sub(16, str(1, "vfs_write"), str(2, "vmlinux"), str(3, "kernel"), varint(6, 3), varint(7, 3)),
		sub(16, str(1, "main.work"), str(2, "app"), str(3, "go"), str(4, "work.go"), varint(5, 12), varint(7, 5)),
		sub(17, varint(1, 1_795_300_000_000), varint(2, 5), str(5, "vfs_write_[k]")),
	)
}
