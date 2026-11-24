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
	GuestdBin  string   // host path to static Linux hotbed-guestd binary
	DNSServers []string // nameservers written to /etc/resolv.conf
	OutTar     string   // destination tar path (must be outside PrivateTmp if cladefsd reads it)
}

// BuildGuestTar pulls ImageRef via go-containerregistry, flattens its layers,
// injects /sbin/hotbed-guestd, /work, and /etc/resolv.conf, and writes a single
// uncompressed tar file suitable for cladefsd.ImportImage without requiring Docker.
func BuildGuestTar(ctx context.Context, opts GuestImageOptions) error {
	if opts.ImageRef == "" {
		return fmt.Errorf("vm image: ImageRef is required")
	}
	if opts.GuestdBin == "" {
		return fmt.Errorf("vm image: GuestdBin is required")
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

	return InjectGuestdTar(rc, opts.GuestdBin, opts.DNSServers, opts.OutTar)
}

// InjectGuestdTar copies all entries from baseTar into outTar while injecting
// /sbin/hotbed-guestd (0755), /work (0755), and /etc/resolv.conf (0644).
// Any pre-existing entry at those exact paths in baseTar is replaced cleanly.
func InjectGuestdTar(baseTar io.Reader, guestdBin string, dnsServers []string, outTar string) error {
	guestdBytes, err := os.ReadFile(guestdBin)
	if err != nil {
		return fmt.Errorf("vm image: read guestd binary %s: %w", guestdBin, err)
	}
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
	hasSbin := false
	hasEtc := false
	sbinDir := "sbin"

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
		case "sbin/hotbed-guestd", "usr/sbin/hotbed-guestd", "etc/resolv.conf":
			// Replace with our injected versions below.
			continue
		case "work", "work/":
			hasWork = true
		case "sbin", "sbin/":
			hasSbin = true
			if hdr.Typeflag == tar.TypeSymlink && hdr.Linkname != "" {
				// Handle usrmerge images (e.g. Debian trixie where sbin -> usr/sbin):
				// cladefsd's tar importer does not traverse symlinks when creating
				// child entries, so write into the resolved target directory.
				target := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(hdr.Linkname, "/"), "./"), "/")
				if target != "" {
					sbinDir = target
				}
			}
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

	if !hasSbin {
		if err := tw.WriteHeader(&tar.Header{Name: "sbin/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return err
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

	// Inject /sbin/hotbed-guestd (or resolved usrmerge target e.g. usr/sbin/hotbed-guestd).
	if err := tw.WriteHeader(&tar.Header{
		Name:     sbinDir + "/hotbed-guestd",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     int64(len(guestdBytes)),
	}); err != nil {
		return fmt.Errorf("vm image: write guestd header: %w", err)
	}
	if _, err := tw.Write(guestdBytes); err != nil {
		return fmt.Errorf("vm image: write guestd body: %w", err)
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
