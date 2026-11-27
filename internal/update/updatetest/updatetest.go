// Package updatetest makes stand-in hotbed binaries, for tests of builds
// and updates.
package updatetest

import (
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"runtime"

	"github.com/semistrict/hotbed/internal/update"
)

// Sum is b's sha256, as SHA256SUMS has it.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Binary is a stand-in hotbed binary that update.Install takes on this
// machine: an executable header for runtime.GOOS/GOARCH, then payload.
func Binary(payload string) []byte {
	return BinaryFor(update.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}, payload)
}

// BinaryFor is a stand-in hotbed binary for platform p.
func BinaryFor(p update.Platform, payload string) []byte {
	hdr := make([]byte, 20)
	switch p.OS {
	case "linux":
		copy(hdr, elf.ELFMAG)
		m := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[p.Arch]
		binary.LittleEndian.PutUint16(hdr[18:20], uint16(m))
	case "darwin":
		binary.LittleEndian.PutUint32(hdr[0:4], macho.Magic64)
		c := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[p.Arch]
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(c))
	default:
		panic("updatetest: no stand-in binary for " + p.String())
	}
	return append(hdr, payload...)
}
