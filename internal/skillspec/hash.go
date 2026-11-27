package skillspec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxFilesPerSkill and MaxTotalSkillBytes match the SEP-2640 interoperability baseline.
	MaxFilesPerSkill   = 512
	MaxTotalSkillBytes = 16 * 1024 * 1024
)

// FileEntry is one file inside a skill package.
type FileEntry struct {
	Path     string // relative POSIX path ("SKILL.md", "references/guide.md")
	Content  []byte
	MIMEType string
	Digest   string // "sha256:<64-lowercase-hex>"
	Binary   bool
}

// SHA256Digest returns the SEP-2640 digest string "sha256:<64-lowercase-hex>".
func SHA256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IsBinary reports whether content contains a NUL byte or invalid UTF-8.
func IsBinary(content []byte) bool {
	if bytes.IndexByte(content, 0) >= 0 {
		return true
	}
	return !utf8.Valid(content)
}

// InferMIMEType returns a sensible MIME type for a skill file path.
func InferMIMEType(relPath string, content []byte) string {
	switch strings.ToLower(path.Ext(relPath)) {
	case ".md", ".markdown", ".mdx":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".toml":
		return "application/toml"
	case ".ts", ".tsx":
		return "text/typescript"
	case ".js", ".mjs", ".cjs":
		return "text/javascript"
	case ".py":
		return "text/x-python"
	case ".sh", ".bash", ".zsh":
		return "text/x-shellscript"
	case ".go":
		return "text/x-go"
	case ".rs":
		return "text/x-rust"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".xml":
		return "application/xml"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	default:
		if IsBinary(content) {
			return "application/octet-stream"
		}
		return "text/plain"
	}
}

func isExcludedEntry(name string, isDir bool) bool {
	if name == "metadata.json" {
		return true
	}
	if isDir && (name == ".git" || name == "__pycache__" || name == "__pypackages__") {
		return true
	}
	return false
}

// HashSkillFiles computes the exact npx skills (skills-lock.json v2) content hash:
// files sorted by UTF-8 path bytes, each framed as `path NUL len NUL content`,
// with CRLF normalized to LF in text files (files without NUL bytes), excluding
// metadata.json and .git / __pycache__ / __pypackages__ directories.
func HashSkillFiles(files []FileEntry) string {
	type normEntry struct {
		path    string
		content []byte
	}
	entries := make([]normEntry, 0, len(files))
	for _, f := range files {
		p := strings.ReplaceAll(f.Path, "\\", "/")
		parts := strings.Split(p, "/")
		skip := false
		for i, part := range parts {
			if isExcludedEntry(part, i < len(parts)-1) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		c := f.Content
		if bytes.IndexByte(c, 0) < 0 && bytes.Contains(c, []byte("\r\n")) {
			c = bytes.ReplaceAll(c, []byte("\r\n"), []byte("\n"))
		}
		entries = append(entries, normEntry{path: p, content: c})
	}
	slices.SortFunc(entries, func(a, b normEntry) int {
		return strings.Compare(a.path, b.path)
	})
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(e.content))))
		h.Write([]byte{0})
		h.Write(e.content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CollectSkillFiles walks skillDir and collects every non-excluded file
// (resolving symlinks safely inside skillDir and avoiding cycles), enforcing
// SEP-2640 baseline file count and byte size limits.
func CollectSkillFiles(skillDir string) ([]FileEntry, error) {
	root, err := filepath.EvalSymlinks(skillDir)
	if err != nil {
		return nil, fmt.Errorf("resolve skill dir %s: %w", skillDir, err)
	}
	root = filepath.Clean(root)
	var out []FileEntry
	var totalBytes int64
	if err := collectDir(skillDir, "", root, map[string]bool{}, &out, &totalBytes); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b FileEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return out, nil
}

func collectDir(dir, prefix, root string, ancestors map[string]bool, out *[]FileEntry, totalBytes *int64) error {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	realDir = filepath.Clean(realDir)
	if ancestors[realDir] {
		return nil
	}
	branch := make(map[string]bool, len(ancestors)+1)
	for k, v := range ancestors {
		branch[k] = v
	}
	branch[realDir] = true

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		isDir := entry.IsDir()
		if isExcludedEntry(name, isDir) {
			continue
		}
		fullPath := filepath.Join(dir, name)
		relPath := prefix + name

		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				continue // skip broken symlink
			}
			target = filepath.Clean(target)
			if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
				continue // skip symlink escaping skill dir
			}
			info, err := os.Stat(target)
			if err != nil {
				continue
			}
			isDir = info.IsDir()
			if isDir && isExcludedEntry(name, true) {
				continue
			}
		} else if !isDir && !entry.Type().IsRegular() {
			continue
		}

		if isDir {
			if err := collectDir(fullPath, relPath+"/", root, branch, out, totalBytes); err != nil {
				return err
			}
			continue
		}

		if len(*out)+1 > MaxFilesPerSkill {
			return fmt.Errorf("skill exceeds maximum of %d files", MaxFilesPerSkill)
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return err
		}
		*totalBytes += int64(len(data))
		if *totalBytes > MaxTotalSkillBytes {
			return fmt.Errorf("skill exceeds maximum total size of %d bytes (16 MiB)", MaxTotalSkillBytes)
		}
		bin := IsBinary(data)
		*out = append(*out, FileEntry{
			Path:     relPath,
			Content:  data,
			MIMEType: InferMIMEType(relPath, data),
			Digest:   SHA256Digest(data),
			Binary:   bin,
		})
	}
	return nil
}
