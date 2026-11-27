// Package update moves hotbed's nodes to new builds. CI publishes each
// build of main as a GitHub release, build-N, with a binary per platform and
// their sums. The controller serves the build it runs to its workers and
// clients, which Install it over themselves. Every node so runs the
// controller's build.
package update

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// ErrRestart is what a node's serve returns once it installed a new build
// over itself: its service manager starts it again, as the new build.
var ErrRestart = errors.New("installed a new build: restart")

// Platform is an OS and an architecture CI builds for.
type Platform struct{ OS, Arch string }

// Platforms are what CI builds: the workers' Linux and the Macs clients run
// on.
var Platforms = []Platform{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Asset is the platform's binary's name in a release and in the cache.
func (p Platform) Asset() string { return "hotbed-" + p.OS + "-" + p.Arch }

// SumsName is a release's sums, in sha256sum's format.
const SumsName = "SHA256SUMS"

// Install puts the binary r reads in place of exe, in one rename, once it
// matches sum and is an executable for this machine's OS and architecture: a
// node handed another platform's build (a Mac worker once got the Linux one)
// keeps the binary it can run rather than replacing it with one it cannot.
func Install(r io.Reader, sum, exe string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".new-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the new binary's sha256 is %s, not %s", got, sum)
	}
	if err := RunsHere(tmp.Name()); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

// RunsHere says whether the file at path is an executable for this machine:
// ELF for Linux, Mach-O for macOS, built for runtime.GOARCH.
func RunsHere(path string) error {
	return runsOn(path, Platform{runtime.GOOS, runtime.GOARCH})
}

func runsOn(path string, want Platform) error {
	got, err := binaryPlatform(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the new binary is built for %s, not for this %s machine", got, want)
	}
	return nil
}

// binaryPlatform reads the platform an executable is built for from its
// header; an architecture hotbed isn't built for reads as "unknown".
func binaryPlatform(path string) (Platform, error) {
	f, err := os.Open(path)
	if err != nil {
		return Platform{}, err
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return Platform{}, fmt.Errorf("the new binary is not an executable: read its header: %w", err)
	}
	arch := func(a string, ok bool) string {
		if !ok {
			return "unknown"
		}
		return a
	}
	switch {
	case bytes.HasPrefix(hdr[:], []byte(elf.ELFMAG)):
		a, ok := elfMachines[elf.Machine(binary.LittleEndian.Uint16(hdr[18:20]))]
		return Platform{"linux", arch(a, ok)}, nil
	case binary.LittleEndian.Uint32(hdr[0:4]) == macho.Magic64:
		a, ok := machoCPUs[macho.Cpu(binary.LittleEndian.Uint32(hdr[4:8]))]
		return Platform{"darwin", arch(a, ok)}, nil
	}
	return Platform{}, errors.New("the new binary is neither an ELF nor a Mach-O executable")
}

var elfMachines = map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}

var machoCPUs = map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}
