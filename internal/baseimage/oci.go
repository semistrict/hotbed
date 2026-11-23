package baseimage

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// PullOCI fetches the Linux OCI image ref for arch ("amd64" or "arm64") and
// unpacks its layers in order into rootDir.
func PullOCI(ctx context.Context, imageRef, arch, rootDir string) error {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}),
	)
	if err != nil {
		return fmt.Errorf("fetch image %s: %w", imageRef, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("list layers of %s: %w", imageRef, err)
	}
	for i, layer := range layers {
		rc, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("open layer %d of %s: %w", i, imageRef, err)
		}
		err = ExtractLayer(rootDir, rc)
		cerr := rc.Close()
		if err != nil {
			return fmt.Errorf("extract layer %d of %s: %w", i, imageRef, err)
		}
		if cerr != nil {
			return fmt.Errorf("close layer %d of %s: %w", i, imageRef, cerr)
		}
	}
	return nil
}

// ExtractLayer unpacks one uncompressed OCI layer tar stream into root,
// applying OCI whiteouts (.wh.<name> and .wh..wh..opq) and rejecting entries
// that escape root or traverse symlinks. Device nodes and FIFOs are skipped
// because MountAPIVFS supplies /dev at runtime.
func ExtractLayer(root string, r io.Reader) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	rfs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rfs.Close()

	tr := tar.NewReader(r)
	isRoot := os.Geteuid() == 0

	type dirMeta struct {
		rel   string
		mode  os.FileMode
		uid   int
		gid   int
		mtime time.Time
	}
	var dirs []dirMeta

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		rel, err := cleanTarPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		base := path.Base(rel)
		parentRel := path.Dir(rel)

		// Handle OCI whiteouts before creating or replacing files.
		if base == whiteoutOpaque {
			if err := clearDirContentsInRoot(rfs, parentRel); err != nil {
				return fmt.Errorf("apply opaque whiteout in %s: %w", parentRel, err)
			}
			continue
		}
		if targetName, ok := strings.CutPrefix(base, whiteoutPrefix); ok {
			if targetName == "" || targetName == "." || targetName == ".." || strings.ContainsRune(targetName, '/') {
				return fmt.Errorf("invalid whiteout entry %q", hdr.Name)
			}
			if err := checkRealDirPath(rfs, parentRel); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("apply whiteout %q: %w", hdr.Name, err)
			}
			if err := removeEntryInRoot(rfs, joinRel(parentRel, targetName)); err != nil {
				return fmt.Errorf("apply whiteout %q: %w", hdr.Name, err)
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllNoSymlink(rfs, rel); err != nil {
				return fmt.Errorf("mkdir %s: %w", rel, err)
			}
			dirs = append(dirs, dirMeta{
				rel:   rel,
				mode:  hdr.FileInfo().Mode().Perm() | specialModeBits(hdr.Mode),
				uid:   hdr.Uid,
				gid:   hdr.Gid,
				mtime: hdr.ModTime,
			})

		case tar.TypeReg, tar.TypeRegA:
			if err := mkdirAllNoSymlink(rfs, parentRel); err != nil {
				return fmt.Errorf("mkdir parent for %s: %w", rel, err)
			}
			// Remove any existing file, symlink, or directory before creating.
			if err := removeEntryInRoot(rfs, rel); err != nil {
				return fmt.Errorf("replace %s: %w", rel, err)
			}
			f, err := rfs.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", rel, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", rel, err)
			}
			if isRoot {
				_ = f.Chown(hdr.Uid, hdr.Gid)
			}
			mode := hdr.FileInfo().Mode().Perm() | specialModeBits(hdr.Mode)
			if err := f.Chmod(mode); err != nil {
				f.Close()
				return fmt.Errorf("chmod %s: %w", rel, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", rel, err)
			}
			if !hdr.ModTime.IsZero() {
				_ = rfs.Chtimes(rel, hdr.ModTime, hdr.ModTime)
			}

		case tar.TypeSymlink:
			if err := mkdirAllNoSymlink(rfs, parentRel); err != nil {
				return fmt.Errorf("mkdir parent for symlink %s: %w", rel, err)
			}
			if err := removeEntryInRoot(rfs, rel); err != nil {
				return fmt.Errorf("replace symlink %s: %w", rel, err)
			}
			if err := rfs.Symlink(hdr.Linkname, rel); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", rel, hdr.Linkname, err)
			}
			if isRoot {
				_ = rfs.Lchown(rel, hdr.Uid, hdr.Gid)
			}

		case tar.TypeLink:
			linkRel, err := cleanTarPath(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", rel, hdr.Linkname, err)
			}
			if linkRel == "" {
				return fmt.Errorf("hardlink %s -> %q: empty target", rel, hdr.Linkname)
			}
			if err := checkRealDirPath(rfs, path.Dir(linkRel)); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", rel, hdr.Linkname, err)
			}
			fi, err := rfs.Lstat(linkRel)
			if err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", rel, hdr.Linkname, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("hardlink %s -> %s: target is a symlink", rel, hdr.Linkname)
			}
			if fi.IsDir() {
				return fmt.Errorf("hardlink %s -> %s: target is a directory", rel, hdr.Linkname)
			}
			if err := mkdirAllNoSymlink(rfs, parentRel); err != nil {
				return fmt.Errorf("mkdir parent for hardlink %s: %w", rel, err)
			}
			if err := removeEntryInRoot(rfs, rel); err != nil {
				return fmt.Errorf("replace hardlink %s: %w", rel, err)
			}
			if err := rfs.Link(linkRel, rel); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", rel, linkRel, err)
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			// Device nodes and FIFOs are omitted (MountAPIVFS supplies /dev).
			continue
		}
	}

	// Apply directory ownership, permissions, and mtimes bottom-up so read-only
	// directories do not block writing their children. Re-verify each path is
	// still a real directory inside root (never a symlink substituted later in
	// the same layer).
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if err := checkRealDirPath(rfs, path.Dir(d.rel)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("verify parent of dir %s: %w", d.rel, err)
		}
		fi, err := rfs.Lstat(d.rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat dir %s: %w", d.rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			continue
		}
		if isRoot {
			_ = rfs.Lchown(d.rel, d.uid, d.gid)
		}
		if err := rfs.Chmod(d.rel, d.mode); err != nil {
			return fmt.Errorf("chmod dir %s: %w", d.rel, err)
		}
		if !d.mtime.IsZero() {
			_ = rfs.Chtimes(d.rel, d.mtime, d.mtime)
		}
	}
	return nil
}

func cleanTarPath(raw string) (string, error) {
	if raw == "" || raw == "." || raw == "./" || raw == "/" {
		return "", nil
	}
	p := strings.TrimPrefix(raw, "/")
	p = path.Clean(p)
	if p == "." || p == "" {
		return "", nil
	}
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("tar entry %q escapes root", raw)
	}
	return p, nil
}

func joinRel(parent, name string) string {
	if parent == "" || parent == "." {
		return name
	}
	return parent + "/" + name
}

// checkRealDirPath verifies that every component of rel inside rfs exists and
// is a real directory, never a symlink. Returns os.ErrNotExist if a component
// does not exist (after confirming all existing ancestor components are real
// directories).
func checkRealDirPath(rfs *os.Root, rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(rel, "/") {
		cur = joinRel(cur, part)
		fi, err := rfs.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q traverses symlink component %q", rel, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("path %q has non-directory component %q", rel, cur)
		}
	}
	return nil
}

// mkdirAllNoSymlink creates each missing component of rel inside rfs as a real
// directory, rejecting any existing symlink or non-directory component.
func mkdirAllNoSymlink(rfs *os.Root, rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(rel, "/") {
		cur = joinRel(cur, part)
		fi, err := rfs.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := rfs.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q traverses symlink component %q", rel, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("path %q has non-directory component %q", rel, cur)
		}
	}
	return nil
}

func clearDirContentsInRoot(rfs *os.Root, parentRel string) error {
	if err := checkRealDirPath(rfs, parentRel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	dirName := parentRel
	if dirName == "" {
		dirName = "."
	}
	dir, err := rfs.Open(dirName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeEntryInRoot(rfs, joinRel(parentRel, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeEntryInRoot(rfs *os.Root, rel string) error {
	fi, err := rfs.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return rfs.Remove(rel)
	}
	return rfs.RemoveAll(rel)
}

func specialModeBits(tarMode int64) os.FileMode {
	var m os.FileMode
	if tarMode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if tarMode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if tarMode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}
