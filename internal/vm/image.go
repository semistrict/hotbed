package vm

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// GuestImageOptions configures how BuildGuestTar constructs a bootable Firecracker
// rootfs tar archive for cladefsd.ImportImage.
type GuestImageOptions struct {
	ImageRef   string   // OCI image reference (e.g. baseimage.DefaultImage)
	Arch       string   // Linux GOARCH ("amd64" or "arm64")
	DNSServers []string // nameservers written to /etc/resolv.conf
	OutTar     string   // destination tar path (must be outside PrivateTmp if cladefsd reads it)
}

// BuildGuestTar pulls ImageRef via go-containerregistry, flattens its layers,
// ensures /work and /etc/resolv.conf, and writes a single uncompressed tar file
// suitable for cladefsd.ImportImage without requiring Docker. hotbed-guestd is
// not in it: VMs boot it from the read-only tools disk.
func BuildGuestTar(ctx context.Context, opts GuestImageOptions) error {
	if opts.ImageRef == "" {
		return fmt.Errorf("vm image: ImageRef is required")
	}
	if opts.OutTar == "" {
		return fmt.Errorf("vm image: OutTar is required")
	}
	arch := opts.Arch
	if arch == "" {
		arch = "amd64"
	}
	ref, err := name.ParseReference(opts.ImageRef)
	if err != nil {
		return fmt.Errorf("vm image: parse %q: %w", opts.ImageRef, err)
	}
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}),
	)
	if err != nil {
		return fmt.Errorf("vm image: pull %q (%s): %w", opts.ImageRef, arch, err)
	}
	rc := mutate.Extract(img)
	defer rc.Close()

	return WriteGuestTar(rc, opts.DNSServers, opts.OutTar)
}

// WriteGuestTar copies all entries from baseTar into outTar while ensuring
// /work (0755) and /etc/resolv.conf (0644). Any pre-existing
// /etc/resolv.conf in baseTar is replaced cleanly.
func WriteGuestTar(baseTar io.Reader, dnsServers []string, outTar string) error {
	if err := os.MkdirAll(filepath.Dir(outTar), 0o755); err != nil {
		return fmt.Errorf("vm image: mkdir %s: %w", filepath.Dir(outTar), err)
	}
	tmpPath := outTar + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("vm image: create %s: %w", tmpPath, err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	tw := tar.NewWriter(f)
	tr := tar.NewReader(baseTar)
	hasWork := false
	hasEtc := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("vm image: read base tar: %w", err)
		}
		norm := strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
		switch norm {
		case "etc/resolv.conf":
			// Replace with our injected version below.
			continue
		case "work", "work/":
			hasWork = true
		case "etc", "etc/":
			hasEtc = true
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("vm image: write header %s: %w", hdr.Name, err)
		}
		if hdr.Size > 0 && (hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA) {
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("vm image: copy %s: %w", hdr.Name, err)
			}
		}
	}

	if !hasEtc {
		if err := tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return err
		}
	}
	if !hasWork {
		if err := tw.WriteHeader(&tar.Header{Name: "work/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return err
		}
	}

	// Inject /etc/resolv.conf
	var resolv strings.Builder
	for _, ns := range dnsServers {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			resolv.WriteString("nameserver " + ns + "\n")
		}
	}
	if resolv.Len() == 0 {
		resolv.WriteString("nameserver 1.1.1.1\nnameserver 8.8.8.8\n")
	}
	resolvBytes := []byte(resolv.String())
	if err := tw.WriteHeader(&tar.Header{
		Name:     "etc/resolv.conf",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(resolvBytes)),
	}); err != nil {
		return fmt.Errorf("vm image: write resolv.conf header: %w", err)
	}
	if _, err := tw.Write(resolvBytes); err != nil {
		return fmt.Errorf("vm image: write resolv.conf body: %w", err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("vm image: close tar: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("vm image: close file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outTar); err != nil {
		return fmt.Errorf("vm image: rename %s -> %s: %w", tmpPath, outTar, err)
	}
	ok = true
	return nil
}
