package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"connectrpc.com/connect/v2"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/gen/guestd/v1/guestdv1connect"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Git sync into the workspace and file browsing of its checkouts.

type syncChunkReader struct {
	stream guestdv1connect.GuestServiceSyncServerStream
	buf    []byte
	total  int64
	limit  int64
}

func (r *syncChunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		msg, err := r.stream.Receive()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		if msg.GetHeader() != nil {
			return 0, connect.NewError(connect.CodeInvalidArgument, "unexpected second sync header")
		}
		chunk := msg.GetChunk()
		if len(chunk) == 0 {
			continue
		}
		r.total += int64(len(chunk))
		if r.total > r.limit {
			return 0, errSyncTooLarge
		}
		r.buf = chunk
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (s *Server) Sync(ctx context.Context, stream guestdv1connect.GuestServiceSyncServerStream) (*guestdv1.SyncResponse, error) {
	first, err := stream.Receive()
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "read sync header: "+err.Error())
	}
	meta := first.GetHeader()
	if meta == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "sync must start with a header")
	}
	if meta.Repo == "" || strings.ContainsAny(meta.Repo, "/\\") || meta.Repo == "." || meta.Repo == ".." {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid repo name %q", meta.Repo))
	}

	var declaredSubBytes int64
	for _, sm := range meta.Subs {
		if sm.BundleBytes < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid submodule bundle size %d", sm.BundleBytes))
		}
		if sm.BundleBytes > maxSyncBundleBytes-declaredSubBytes {
			return nil, connect.NewError(connect.CodeResourceExhausted, errSyncTooLarge.Error())
		}
		declaredSubBytes += sm.BundleBytes
	}

	tmpDir, err := os.MkdirTemp(s.syncDir, "apply-*")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "create sync tmp dir: "+err.Error())
	}
	defer os.RemoveAll(tmpDir)

	reader := &syncChunkReader{stream: stream, limit: maxSyncBundleBytes}
	subs := make([]gitops.SubCheckout, 0, len(meta.Subs))
	var subFiles []string

	for i, sm := range meta.Subs {
		sub := gitops.SubCheckout{Path: sm.Path, Commit: sm.Commit}
		if sm.BundleBytes > 0 {
			dstPath := filepath.Join(tmpDir, fmt.Sprintf("sub-%d.bundle", i))
			f, err := os.Create(dstPath)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, "create submodule bundle file: "+err.Error())
			}
			_, copyErr := io.CopyN(f, reader, sm.BundleBytes)
			closeErr := f.Close()
			if copyErr != nil || closeErr != nil {
				if errors.Is(copyErr, errSyncTooLarge) {
					return nil, connect.NewError(connect.CodeResourceExhausted, errSyncTooLarge.Error())
				}
				if errors.Is(copyErr, io.EOF) || errors.Is(copyErr, io.ErrUnexpectedEOF) {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("truncated submodule bundle %q", sm.Path))
				}
				return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("write submodule bundle %s: %v", sm.Path, errors.Join(copyErr, closeErr)))
			}
			sub.Bundle = dstPath
			subFiles = append(subFiles, dstPath)
		}
		subs = append(subs, sub)
	}

	mainBundle := filepath.Join(tmpDir, "bundle.bundle")
	bf, err := os.Create(mainBundle)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "create main bundle file: "+err.Error())
	}
	_, copyErr := io.Copy(bf, reader)
	closeErr := bf.Close()
	if copyErr != nil || closeErr != nil {
		if errors.Is(copyErr, errSyncTooLarge) {
			return nil, connect.NewError(connect.CodeResourceExhausted, errSyncTooLarge.Error())
		}
		var ce *connect.Error
		if errors.As(copyErr, &ce) {
			return nil, ce
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("write main bundle: %v", errors.Join(copyErr, closeErr)))
	}

	repoDir := filepath.Join(s.workDir, meta.Repo)
	g := gitops.Git{Dir: repoDir, Env: meta.Env}
	if (meta.Uid > 0 || meta.Gid > 0) && os.Geteuid() == 0 {
		// guestd runs as root (PID 1 in a Firecracker VM) and created s.syncDir /
		// tmpDir / bundle files as root; ensure the unprivileged git user can
		// traverse to and read the bundle files.
		_ = os.Chmod(s.stateDir, 0o755)
		_ = os.Chmod(s.syncDir, 0o755)
		_ = os.Chown(tmpDir, int(meta.Uid), int(meta.Gid))
		_ = os.Chmod(tmpDir, 0o755)
		_ = os.Chown(mainBundle, int(meta.Uid), int(meta.Gid))
		for _, p := range subFiles {
			_ = os.Chown(p, int(meta.Uid), int(meta.Gid))
		}
		g.Command = func(ctx context.Context, dir string, args []string) (*exec.Cmd, error) {
			c := exec.CommandContext(ctx, "git", args...)
			c.Dir = dir
			c.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{
					Uid: uint32(meta.Uid),
					Gid: uint32(meta.Gid),
				},
			}
			return c, nil
		}
	}

	changed, applyErr := gitops.Apply(ctx, g, mainBundle, meta.Commit, meta.Base, subs)
	resp := &guestdv1.SyncResponse{FilesChanged: int32(changed)}
	if applyErr != nil {
		var ae *gitops.ApplyError
		if errors.As(applyErr, &ae) {
			resp.Error = ae.Err.Error()
			resp.Mutated = ae.Mutated
			resp.RolledBack = ae.RolledBack
			if ae.RollbackErr != nil {
				resp.RollbackError = ae.RollbackErr.Error()
			}
		} else {
			resp.Error = applyErr.Error()
		}
	}
	return resp, nil
}

func (s *Server) ListFiles(ctx context.Context, req *guestdv1.ListFilesRequest) (*guestdv1.ListFilesResponse, error) {
	workRoot, err := os.OpenRoot(s.workDir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "open workDir: "+err.Error())
	}
	defer workRoot.Close()

	var out []*pb.FileEntry
	for _, repo := range req.GetRepos() {
		if repo.Repo == "" || strings.ContainsAny(repo.Repo, "/\\") || repo.Repo == "." || repo.Repo == ".." {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid repo %q", repo.Repo))
		}
		repoRoot, err := workRoot.OpenRoot(repo.Repo)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Sprintf("open repo %s: %v", repo.Repo, err))
		}
		dir := filepath.Join(s.workDir, repo.Repo)
		entries, err := gitops.ListRepoFiles(ctx, repoGit(dir), repoRoot, repo.BaseCommit)
		_ = repoRoot.Close()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("list %s: %v", repo.Repo, err))
		}
		out = append(out, &pb.FileEntry{Path: repo.Repo, Kind: "dir"})
		for _, f := range entries {
			f.Path = repo.Repo + "/" + f.Path
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return &guestdv1.ListFilesResponse{Files: out}, nil
}

// repoGit runs git in the checkout dir: guestd runs as root while the
// checkouts are the build user's, so safe.directory admits them.
func repoGit(dir string) gitops.Git {
	return gitops.Git{Dir: dir, Env: []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*"}}
}

func (s *Server) GetFile(ctx context.Context, req *guestdv1.GetFileRequest) (*pb.FileContent, error) {
	if req.Repo == "" || strings.ContainsAny(req.Repo, "/\\") || req.Repo == "." || req.Repo == ".." || req.RelPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "invalid repo or relPath")
	}
	workRoot, err := os.OpenRoot(s.workDir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "open workDir: "+err.Error())
	}
	defer workRoot.Close()

	repoRoot, err := workRoot.OpenRoot(req.Repo)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeNotFound, err.Error())
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	defer repoRoot.Close()

	data, err := gitops.ReadRepoFile(repoRoot, req.RelPath, maxFileBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, gitops.ErrNotRegularFile):
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%s is not a regular file", req.RelPath))
	case err != nil:
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}

	var changed []int32
	if req.BaseCommit != "" {
		changed, err = gitops.FileChangedLines(ctx, repoGit(filepath.Join(s.workDir, req.Repo)), req.RelPath, req.BaseCommit, data)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, "git diff: "+err.Error())
		}
	}
	return &pb.FileContent{
		Path:         req.Repo + "/" + req.RelPath,
		Text:         string(data),
		ChangedLines: changed,
	}, nil
}
