package gitops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The reads below serve a workspace's files to people looking at them (the
// worker for a host checkout, guestd for one in a VM). Each is confined to
// an *os.Root opened on the checkout, and none trusts what the checkout
// holds: its files are an agent's, and its git may be one the agent built.

// ListRepoFiles lists the files git tracks in g.Dir, the checkout repo is
// opened on, relative to it, and the directories that hold them. Each file
// is marked by how it differs from commit base ("added", "modified"); none
// is marked when base is "".
func ListRepoFiles(ctx context.Context, g Git, repo *os.Root, base string) ([]*pb.FileEntry, error) {
	list, err := g.Run(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	changes := map[string]string{}
	if base != "" {
		status, err := g.Run(ctx, "diff", "--name-status", "-z", base)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(status, "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			switch parts[i] {
			case "A":
				changes[parts[i+1]] = "added"
			case "M":
				changes[parts[i+1]] = "modified"
			}
		}
	}
	dirs := map[string]bool{}
	var out []*pb.FileEntry
	for _, p := range strings.Split(list, "\x00") {
		if p == "" {
			continue
		}
		var size int64
		if st, err := repo.Lstat(p); err == nil {
			size = st.Size()
		}
		out = append(out, &pb.FileEntry{Path: p, Kind: "file", SizeBytes: size, Change: changes[p]})
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		out = append(out, &pb.FileEntry{Path: d, Kind: "dir"})
	}
	return out, nil
}

// ErrNotRegularFile is a path that names a FIFO, socket, device or
// directory where a regular file was wanted.
var ErrNotRegularFile = errors.New("not a regular file")

// OpenRegularFile opens rel in root for reading, or fails with
// ErrNotRegularFile if it is not a regular file (or a symlink to one). It
// never blocks on a special file: one swapped in between the check and the
// open (O_NONBLOCK) is caught by the second check, on what was opened.
func OpenRegularFile(root *os.Root, rel string) (*os.File, error) {
	fi, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	ffi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ffi.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotRegularFile
	}
	return f, nil
}

// ReadRepoFile reads at most limit bytes of the regular file rel in root
// (OpenRegularFile).
func ReadRepoFile(root *os.Root, rel string, limit int64) ([]byte, error) {
	f, err := OpenRegularFile(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// FileChangedLines is which lines of text, the content of rel in the
// checkout g.Dir, differ from commit base: by git diff -U0 (ParseChangedLines).
func FileChangedLines(ctx context.Context, g Git, rel, base string, text []byte) ([]int32, error) {
	diff, err := g.Run(ctx, "diff", "-U0", base, "--", rel)
	if err != nil {
		return nil, err
	}
	return ParseChangedLines(diff, CountLines(text)), nil
}

var diffHunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// ParseChangedLines reads the new side's line numbers from a -U0 diff of a
// file of n lines: those in the file, once each, in order. The diff is a git
// the checkout's owner may have replaced (board 8333): a hunk claims no line
// the file does not have, and the work is the diff's and the file's size,
// whatever the hunks claim.
func ParseChangedLines(diff string, n int) []int32 {
	// starts[l] - ends[l]: the hunks open over line l, by prefix sums.
	delta := make([]int, n+2)
	// By newlines, not a bufio.Scanner: a line of a minified file is past
	// its limit, and every hunk after it would be lost.
	for line := range strings.SplitSeq(diff, "\n") {
		m := diffHunk.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		count := 1
		if m[2] != "" {
			if count, err = strconv.Atoi(m[2]); err != nil {
				continue
			}
		}
		lo, hi := max(start, 1), min(start+count-1, n)
		if count <= 0 || lo > hi {
			continue
		}
		delta[lo]++
		delta[hi+1]--
	}
	lines := []int32{}
	open := 0
	for l := 1; l <= n; l++ {
		open += delta[l]
		if open > 0 {
			lines = append(lines, int32(l))
		}
	}
	return lines
}

// CountLines is how many lines text has, a last one without a newline
// included.
func CountLines(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// GitCredentialHelperScript is the credential helper git runs (through
// credential.helper) in a workspace: it posts git's key=value attributes to
// the git-auth socket named by $HOTBED_GIT_CRED_SOCK, or defaultSock where
// that is unset ("" for none), and writes the credentials it answers to
// stdout, never to disk.
func GitCredentialHelperScript(defaultSock string) string {
	sock := `"$HOTBED_GIT_CRED_SOCK"`
	if defaultSock != "" {
		sock = `"${HOTBED_GIT_CRED_SOCK:-` + defaultSock + `}"`
	}
	return `#!/bin/sh
case "${1:-}" in
  get)   path=/git-credential ;;
  erase) path=/git-credential/erase ;;
  *)     exit 0 ;;
esac
out=$(curl -sS --unix-socket ` + sock + ` -w '\n%{http_code}' --data-binary @- "http://localhost$path") || exit 1
nl='
'
code=${out##*"$nl"}
body=${out%"$nl"*}
if [ "$code" = "200" ]; then
  printf '%s\n' "$body"
  exit 0
fi
if [ "$code" = "204" ]; then
  exit 0
fi
printf '%s\n' "$body" >&2
exit 1
`
}
