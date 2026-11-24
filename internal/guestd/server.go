package guestd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/pty"
)

const (
	maxFileBytes       = 1 << 20
	maxSyncBundleBytes = 2 << 30
)

var validCmdID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// ServerConfig configures a guestd.Server.
type ServerConfig struct {
	WorkDir  string // workspace root containing repo checkouts (default /work)
	StateDir string // runtime dir for logs and temp sync bundles (default /run/hotbed-guestd)
	Logger   *slog.Logger
}

// Server runs inside a workspace VM and handles requests from hotbed-worker.
type Server struct {
	workDir  string
	stateDir string
	logDir   string
	syncDir  string
	log      *slog.Logger

	mu   sync.Mutex
	cmds map[string]*managedCmd
}

type managedCmd struct {
	mu      sync.Mutex
	id      string
	cmd     *exec.Cmd
	cg      *cmdCgroup
	logPath string
	logFile *os.File
	state   string
	exit    int
	mainPID int
	started int64
	exited  int64
	errMsg  string
	done    chan struct{}
}

// NewServer constructs a guestd Server and prepares its state directories.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/work"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/run/hotbed-guestd"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	logDir := filepath.Join(cfg.StateDir, "logs")
	syncDir := filepath.Join(cfg.StateDir, "sync")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guestd log dir: %w", err)
	}
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guestd sync dir: %w", err)
	}
	return &Server{
		workDir:  cfg.WorkDir,
		stateDir: cfg.StateDir,
		logDir:   logDir,
		syncDir:  syncDir,
		log:      cfg.Logger,
		cmds:     map[string]*managedCmd{},
	}, nil
}

// Handler returns the HTTP handler for ControlPort.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/commands/start", s.handleStartCommand)
	mux.HandleFunc("POST /v1/commands/status", s.handleStatus)
	mux.HandleFunc("POST /v1/commands/stop", s.handleStopCommand)
	mux.HandleFunc("POST /v1/commands/freeze", s.handleFreezeCommand)
	mux.HandleFunc("POST /v1/commands/thaw", s.handleThawCommand)
	mux.HandleFunc("POST /v1/reclaim", s.handleReclaim)
	mux.HandleFunc("GET /v1/commands/logs", s.handleStreamLogs)
	mux.HandleFunc("POST /v1/sync", s.handleSync)
	mux.HandleFunc("POST /v1/files/list", s.handleListFiles)
	mux.HandleFunc("POST /v1/files/get", s.handleGetFile)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	return mux
}

// ServeControl serves the HTTP control API on ln until ctx is cancelled.
func (s *Server) ServeControl(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// resolveWorkDir maps "/work" (and "/work/<subpath>") onto s.workDir when
// running in a custom work directory (e.g. non-root host tests). Inside a VM
// where s.workDir == "/work", this is an identity mapping.
func (s *Server) resolveWorkDir(dir string) string {
	if dir == "" {
		return s.workDir
	}
	if s.workDir != "/work" {
		if dir == "/work" {
			return s.workDir
		}
		if rest, ok := strings.CutPrefix(dir, "/work/"); ok {
			return filepath.Join(s.workDir, rest)
		}
	}
	return dir
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, HealthResponse{
		Hostname: host,
		Kernel:   kernelRelease(),
		Arch:     runtime.GOARCH,
		PID1:     os.Getpid() == 1,
		CgroupV2: hasCgroupV2(),
		WorkDir:  s.workDir,
	})
}

func (s *Server) handleStartCommand(w http.ResponseWriter, r *http.Request) {
	var req StartCommandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode start request: "+err.Error())
		return
	}
	if !validCmdID.MatchString(req.ID) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid command id %q", req.ID))
		return
	}
	if len(req.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "argv is required")
		return
	}

	// Idempotent start: if ID already exists, return its current status.
	s.mu.Lock()
	if existing, ok := s.cmds[req.ID]; ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, existing.snapshot())
		return
	}

	logPath := filepath.Join(s.logDir, req.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "open log file: "+err.Error())
		return
	}

	dir := s.resolveWorkDir(req.Dir)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	if req.UID > 0 || req.GID > 0 {
		if os.Geteuid() == 0 {
			attr.Credential = &syscall.Credential{
				Uid: uint32(req.UID),
				Gid: uint32(req.GID),
			}
		}
	}
	cmd.SysProcAttr = attr

	cg := createCmdCgroup(req.ID, req)
	usedCgroupFD, closeCgroupFD := attachCgroupAttr(attr, cg)
	err = cmd.Start()
	closeCgroupFD()
	if err != nil {
		_ = logFile.Close()
		if cg != nil {
			cg.remove()
		}
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "start command: "+err.Error())
		return
	}
	pid := cmd.Process.Pid
	if cg != nil && !usedCgroupFD {
		_ = cg.addPID(pid)
	}

	mc := &managedCmd{
		id:      req.ID,
		cmd:     cmd,
		cg:      cg,
		logPath: logPath,
		logFile: logFile,
		state:   StateRunning,
		mainPID: pid,
		started: time.Now().UnixMilli(),
		done:    make(chan struct{}),
	}
	s.cmds[req.ID] = mc
	s.mu.Unlock()

	go s.waitCommand(mc)

	writeJSON(w, http.StatusOK, mc.snapshot())
}

func (s *Server) waitCommand(mc *managedCmd) {
	err := mc.cmd.Wait()
	code := exitCodeOf(err)
	mc.mu.Lock()
	mc.state = StateExited
	mc.exit = code
	mc.exited = time.Now().UnixMilli()
	if err != nil && code == 0 {
		mc.errMsg = err.Error()
	}
	_ = mc.logFile.Close()
	if mc.cg != nil {
		mc.cg.killAll()
		mc.cg.remove()
	}
	close(mc.done)
	mc.mu.Unlock()
}

func (mc *managedCmd) snapshot() CommandStatus {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	var logBytes int64
	if st, err := os.Stat(mc.logPath); err == nil {
		logBytes = st.Size()
	}
	var cgPath string
	var memAnon, memSwap, cpuUsec int64
	if mc.cg != nil {
		cgPath = mc.cg.path
		if mc.state != StateExited {
			memAnon, memSwap, cpuUsec = mc.cg.stats()
		}
	}
	return CommandStatus{
		ID:         mc.id,
		State:      mc.state,
		ExitCode:   mc.exit,
		MainPID:    mc.mainPID,
		Cgroup:     cgPath,
		MemoryAnon: memAnon,
		MemorySwap: memSwap,
		CPUUsec:    cpuUsec,
		LogBytes:   logBytes,
		StartedMs:  mc.started,
		ExitedMs:   mc.exited,
		Error:      mc.errMsg,
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var req StatusRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "decode status request: "+err.Error())
		return
	}
	s.mu.Lock()
	var list []*managedCmd
	if len(req.IDs) == 0 {
		for _, mc := range s.cmds {
			list = append(list, mc)
		}
	} else {
		for _, id := range req.IDs {
			if mc, ok := s.cmds[id]; ok {
				list = append(list, mc)
			}
		}
	}
	s.mu.Unlock()

	out := make(map[string]CommandStatus, len(list))
	for _, mc := range list {
		out[mc.id] = mc.snapshot()
	}
	writeJSON(w, http.StatusOK, StatusResponse{Commands: out})
}

func (s *Server) getCmd(id string) (*managedCmd, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mc, ok := s.cmds[id]
	return mc, ok
}

func (s *Server) handleStopCommand(w http.ResponseWriter, r *http.Request) {
	var req StopCommandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode stop request: "+err.Error())
		return
	}
	mc, ok := s.getCmd(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown command "+req.ID)
		return
	}
	grace := time.Duration(req.GraceMs) * time.Millisecond
	if grace <= 0 {
		grace = 3 * time.Second
	}
	s.stopManagedCmd(mc, grace)
	writeJSON(w, http.StatusOK, mc.snapshot())
}

func (s *Server) stopManagedCmd(mc *managedCmd, grace time.Duration) {
	mc.mu.Lock()
	if mc.state == StateExited {
		mc.mu.Unlock()
		return
	}
	pid := mc.mainPID
	cg := mc.cg
	wasFrozen := mc.state == StateFrozen
	mc.mu.Unlock()

	if wasFrozen {
		if cg != nil {
			_ = cg.thaw()
		} else {
			_ = signalProcessGroup(pid, syscall.SIGCONT)
		}
	}
	_ = signalProcessGroup(pid, syscall.SIGTERM)

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-mc.done:
		return
	case <-timer.C:
		if cg != nil {
			cg.killAll()
		}
		_ = signalProcessGroup(pid, syscall.SIGKILL)
		<-mc.done
	}
}

func (s *Server) handleFreezeCommand(w http.ResponseWriter, r *http.Request) {
	var req FreezeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode freeze request: "+err.Error())
		return
	}
	mc, ok := s.getCmd(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown command "+req.ID)
		return
	}
	mc.mu.Lock()
	if mc.state == StateExited {
		mc.mu.Unlock()
		writeJSON(w, http.StatusOK, mc.snapshot())
		return
	}
	var err error
	if mc.cg != nil {
		err = mc.cg.freeze()
	} else {
		err = signalProcessGroup(mc.mainPID, syscall.SIGSTOP)
	}
	if err == nil {
		mc.state = StateFrozen
	}
	mc.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "freeze command: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mc.snapshot())
}

func (s *Server) handleThawCommand(w http.ResponseWriter, r *http.Request) {
	var req FreezeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode thaw request: "+err.Error())
		return
	}
	mc, ok := s.getCmd(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown command "+req.ID)
		return
	}
	mc.mu.Lock()
	if mc.state == StateExited {
		mc.mu.Unlock()
		writeJSON(w, http.StatusOK, mc.snapshot())
		return
	}
	var err error
	if mc.cg != nil {
		err = mc.cg.thaw()
	} else {
		err = signalProcessGroup(mc.mainPID, syscall.SIGCONT)
	}
	if err == nil {
		mc.state = StateRunning
	}
	mc.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "thaw command: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mc.snapshot())
}

func (s *Server) handleReclaim(w http.ResponseWriter, r *http.Request) {
	var req ReclaimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "decode reclaim request: "+err.Error())
		return
	}
	if req.ID != "" && req.Bytes > 0 {
		if mc, ok := s.getCmd(req.ID); ok && mc.cg != nil {
			_ = mc.cg.reclaim(req.Bytes)
		}
	}
	_ = dropPageCaches()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStreamLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	mc, ok := s.getCmd(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown command "+id)
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	follow := r.URL.Query().Get("follow") == "1"

	f, err := os.Open(mc.logPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open log: "+err.Error())
		return
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			writeErr(w, http.StatusBadRequest, "seek log: "+err.Error())
			return
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			if !follow {
				return
			}
			select {
			case <-mc.done:
				// Drain any final bytes written before close.
				for {
					n, err := f.Read(buf)
					if n > 0 {
						_, _ = w.Write(buf[:n])
						if flusher != nil {
							flusher.Flush()
						}
					}
					if err != nil {
						return
					}
				}
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if rerr != nil {
			return
		}
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart sync body: "+err.Error())
		return
	}
	tmpDir, err := os.MkdirTemp(s.syncDir, "apply-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create sync tmp dir: "+err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)

	var (
		meta       SyncRequest
		haveMeta   bool
		mainBundle string
		partFiles  = map[string]string{}
		totalBytes int64
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read multipart sync: "+err.Error())
			return
		}
		name := part.FormName()
		switch name {
		case "meta":
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&meta); err != nil {
				_ = part.Close()
				writeErr(w, http.StatusBadRequest, "decode sync meta: "+err.Error())
				return
			}
			haveMeta = true
		default:
			if name == "" || strings.ContainsAny(name, "/\\") {
				_ = part.Close()
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid sync part name %q", name))
				return
			}
			dstPath := filepath.Join(tmpDir, name+".bundle")
			f, err := os.Create(dstPath)
			if err != nil {
				_ = part.Close()
				writeErr(w, http.StatusInternalServerError, "create bundle file: "+err.Error())
				return
			}
			n, copyErr := io.Copy(f, io.LimitReader(part, maxSyncBundleBytes-totalBytes+1))
			closeErr := f.Close()
			_ = part.Close()
			if copyErr != nil || closeErr != nil {
				writeErr(w, http.StatusInternalServerError, fmt.Sprintf("write bundle %s: %v", name, errors.Join(copyErr, closeErr)))
				return
			}
			totalBytes += n
			if totalBytes > maxSyncBundleBytes {
				writeErr(w, http.StatusRequestEntityTooLarge, "sync bundle exceeds size limit")
				return
			}
			if name == "bundle" {
				mainBundle = dstPath
			} else {
				partFiles[name] = dstPath
			}
		}
		_ = part.Close()
	}
	if !haveMeta {
		writeErr(w, http.StatusBadRequest, "missing sync meta part")
		return
	}
	if meta.Repo == "" || strings.ContainsAny(meta.Repo, "/\\") || meta.Repo == "." || meta.Repo == ".." {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid repo name %q", meta.Repo))
		return
	}
	if mainBundle == "" {
		writeErr(w, http.StatusBadRequest, "missing main bundle part")
		return
	}

	subs := make([]gitops.SubCheckout, 0, len(meta.Subs))
	for _, sm := range meta.Subs {
		sub := gitops.SubCheckout{Path: sm.Path, Commit: sm.Commit}
		if sm.Part != "" {
			p, ok := partFiles[sm.Part]
			if !ok {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("missing submodule bundle part %q", sm.Part))
				return
			}
			sub.Bundle = p
		}
		subs = append(subs, sub)
	}

	repoDir := filepath.Join(s.workDir, meta.Repo)
	g := gitops.Git{Dir: repoDir, Env: meta.Env}
	if (meta.UID > 0 || meta.GID > 0) && os.Geteuid() == 0 {
		// guestd runs as root (PID 1 in a Firecracker VM) and created s.syncDir /
		// tmpDir / bundle files as root; ensure the unprivileged git user can
		// traverse to and read the bundle files.
		_ = os.Chmod(s.stateDir, 0o755)
		_ = os.Chmod(s.syncDir, 0o755)
		_ = os.Chown(tmpDir, meta.UID, meta.GID)
		_ = os.Chmod(tmpDir, 0o755)
		_ = os.Chown(mainBundle, meta.UID, meta.GID)
		for _, p := range partFiles {
			_ = os.Chown(p, meta.UID, meta.GID)
		}
		g.Command = func(ctx context.Context, dir string, args []string) (*exec.Cmd, error) {
			c := exec.CommandContext(ctx, "git", args...)
			c.Dir = dir
			c.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{
					Uid: uint32(meta.UID),
					Gid: uint32(meta.GID),
				},
			}
			return c, nil
		}
	}

	changed, applyErr := gitops.Apply(r.Context(), g, mainBundle, meta.Commit, meta.Base, subs)
	resp := SyncResponse{FilesChanged: changed}
	if applyErr != nil {
		var ae *gitops.ApplyError
		if errors.As(applyErr, &ae) {
			resp.Err = ae.Err.Error()
			resp.Mutated = ae.Mutated
			resp.RolledBack = ae.RolledBack
			if ae.RollbackErr != nil {
				resp.RollbackErr = ae.RollbackErr.Error()
			}
		} else {
			resp.Err = applyErr.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	var req ListFilesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode list files request: "+err.Error())
		return
	}
	workRoot, err := os.OpenRoot(s.workDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open workDir: "+err.Error())
		return
	}
	defer workRoot.Close()

	var out []*pb.FileEntry
	for _, repo := range req.Repos {
		if repo.Repo == "" || strings.ContainsAny(repo.Repo, "/\\") || repo.Repo == "." || repo.Repo == ".." {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid repo %q", repo.Repo))
			return
		}
		repoRoot, err := workRoot.OpenRoot(repo.Repo)
		if err != nil {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("open repo %s: %v", repo.Repo, err))
			return
		}
		dir := filepath.Join(s.workDir, repo.Repo)
		entries, err := listRepoFiles(r.Context(), repoRoot, dir, repo.BaseCommit)
		_ = repoRoot.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("list %s: %v", repo.Repo, err))
			return
		}
		out = append(out, &pb.FileEntry{Path: repo.Repo, Kind: "dir"})
		for _, f := range entries {
			f.Path = repo.Repo + "/" + f.Path
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, http.StatusOK, ListFilesResponse{Files: out})
}

func listRepoFiles(ctx context.Context, repoRoot *os.Root, dir, base string) ([]*pb.FileEntry, error) {
	g := gitops.Git{Dir: dir}
	// hotbed-guestd runs as root while workspace repos are owned by the non-root
	// build user; pass safe.directory=* so git does not reject dubious ownership.
	list, err := g.Run(ctx, "-c", "safe.directory=*", "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	changes := map[string]string{}
	if base != "" {
		status, err := g.Run(ctx, "-c", "safe.directory=*", "diff", "--name-status", "-z", base)
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
		if st, err := repoRoot.Lstat(p); err == nil {
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

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	var req GetFileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "decode get file request: "+err.Error())
		return
	}
	if req.Repo == "" || strings.ContainsAny(req.Repo, "/\\") || req.Repo == "." || req.Repo == ".." || req.RelPath == "" {
		writeErr(w, http.StatusBadRequest, "invalid repo or relPath")
		return
	}
	workRoot, err := os.OpenRoot(s.workDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open workDir: "+err.Error())
		return
	}
	defer workRoot.Close()

	repoRoot, err := workRoot.OpenRoot(req.Repo)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer repoRoot.Close()

	st, err := repoRoot.Lstat(req.RelPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !st.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s is not a regular file", req.RelPath))
		return
	}
	f, err := repoRoot.Open(req.RelPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var changed []int32
	if req.BaseCommit != "" {
		g := gitops.Git{Dir: filepath.Join(s.workDir, req.Repo)}
		diff, err := g.Run(r.Context(), "-c", "safe.directory=*", "diff", "-U0", req.BaseCommit, "--", req.RelPath)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "git diff: "+err.Error())
			return
		}
		changed = parseChangedLines(diff, countLines(data))
	}
	writeJSON(w, http.StatusOK, GetFileResponse{
		Path:         req.Repo + "/" + req.RelPath,
		Text:         string(data),
		ChangedLines: changed,
	})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	var req ShutdownRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "decode shutdown request: "+err.Error())
		return
	}
	if req.StopCommands {
		s.mu.Lock()
		all := make([]*managedCmd, 0, len(s.cmds))
		for _, mc := range s.cmds {
			all = append(all, mc)
		}
		s.mu.Unlock()
		for _, mc := range all {
			s.stopManagedCmd(mc, 2*time.Second)
		}
	}
	if req.SyncFS {
		syncFilesystems()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if req.PowerOff {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = powerOffGuest()
		}()
	}
}

// ServeShell accepts interactive shell connections on ln (ShellPort) until ctx ends.
func (s *Server) ServeShell(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleShellConn(ctx, conn)
	}
}

func (s *Server) handleShellConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req ShellOpenRequest
	if err := readJSONFrame(conn, frameOpen, &req); err != nil {
		_ = writeFrame(conn, frameError, []byte(err.Error()))
		return
	}
	argv := req.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/bash", "-l"}
	}
	dir := s.resolveWorkDir(req.Dir)

	var writeMu sync.Mutex
	send := func(typ byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeFrame(conn, typ, payload)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	env := append([]string(nil), req.Env...)
	if req.Term != "" {
		env = append(env, "TERM="+req.Term)
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	attr := &syscall.SysProcAttr{}
	if (req.UID > 0 || req.GID > 0) && os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{
			Uid: uint32(req.UID),
			Gid: uint32(req.GID),
		}
	}

	if req.PTY {
		master, slave, _, err := pty.Open()
		if err != nil {
			_ = send(frameError, []byte("open pty: "+err.Error()))
			return
		}
		cols, rows := req.Cols, req.Rows
		if cols == 0 {
			cols = 80
		}
		if rows == 0 {
			rows = 24
		}
		_ = pty.Resize(master, cols, rows)

		attr.Setsid = true
		attr.Setctty = true
		cmd.SysProcAttr = attr
		cmd.Stdin = slave
		cmd.Stdout = slave
		cmd.Stderr = slave
		if err := cmd.Start(); err != nil {
			_ = slave.Close()
			_ = master.Close()
			_ = send(frameError, []byte("start shell: "+err.Error()))
			return
		}
		_ = slave.Close()
		pid := cmd.Process.Pid

		// Pump PTY master -> host.
		outDone := make(chan struct{})
		go func() {
			defer close(outDone)
			buf := make([]byte, 16<<10)
			for {
				n, err := master.Read(buf)
				if n > 0 {
					if send(frameData, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// Pump host -> PTY master & handle resize frames.
		go func() {
			for {
				typ, payload, err := readFrame(conn)
				if err != nil {
					_ = signalProcessGroup(pid, syscall.SIGHUP)
					_ = master.Close()
					return
				}
				switch typ {
				case frameData:
					if len(payload) > 0 {
						_, _ = master.Write(payload)
					}
				case frameResize:
					if len(payload) == 4 {
						c := binary.BigEndian.Uint16(payload[0:2])
						rw := binary.BigEndian.Uint16(payload[2:4])
						_ = pty.Resize(master, c, rw)
					}
				case frameCloseStdin:
					// PTY has no half-close; send EOT if needed or ignore.
				}
			}
		}()

		waitErr := cmd.Wait()
		// Drain buffered PTY output before closing master: once the child
		// closes the slave fd, master.Read returns the remaining bytes and
		// then EIO/EOF. Bound the wait in case a grandchild kept the slave open.
		select {
		case <-outDone:
		case <-time.After(250 * time.Millisecond):
			_ = master.Close()
			<-outDone
		}
		_ = master.Close()
		var code [4]byte
		binary.BigEndian.PutUint32(code[:], uint32(int32(exitCodeOf(waitErr))))
		_ = send(frameExit, code[:])
		return
	}

	// Piped (non-PTY) shell.
	attr.Setpgid = true
	cmd.SysProcAttr = attr
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	if err := cmd.Start(); err != nil {
		_ = send(frameError, []byte("start shell: "+err.Error()))
		return
	}
	pid := cmd.Process.Pid

	var outWG sync.WaitGroup
	pump := func(r io.Reader, typ byte) {
		defer outWG.Done()
		buf := make([]byte, 16<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if send(typ, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	outWG.Add(2)
	go pump(stdoutPipe, frameData)
	go pump(stderrPipe, frameStderr)

	go func() {
		for {
			typ, payload, err := readFrame(conn)
			if err != nil {
				_ = stdinPipe.Close()
				_ = signalProcessGroup(pid, syscall.SIGTERM)
				return
			}
			switch typ {
			case frameData:
				if len(payload) > 0 {
					_, _ = stdinPipe.Write(payload)
				}
			case frameCloseStdin:
				_ = stdinPipe.Close()
			}
		}
	}()

	outWG.Wait()
	waitErr := cmd.Wait()
	var code [4]byte
	binary.BigEndian.PutUint32(code[:], uint32(int32(exitCodeOf(waitErr))))
	_ = send(frameExit, code[:])
}

// ServeTCPProxy accepts host-initiated dev-server proxy connections on ln
// (TCPProxyPort), dials 127.0.0.1:<port> inside the guest, and bridges bytes.
func (s *Server) ServeTCPProxy(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleTCPProxyConn(conn)
	}
}

func handleTCPProxyConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var req TCPDialRequest
	if err := json.Unmarshal(line, &req); err != nil || req.Port == 0 {
		_ = json.NewEncoder(conn).Encode(TCPDialResponse{OK: false, Err: "invalid port"})
		return
	}
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port), 5*time.Second)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(TCPDialResponse{OK: false, Err: err.Error()})
		return
	}
	defer target.Close()
	if err := json.NewEncoder(conn).Encode(TCPDialResponse{OK: true}); err != nil {
		return
	}
	bridgeConns(&bufferedConn{Conn: conn, r: br}, target)
}

const guestGitCredentialHelper = `#!/bin/sh
case "${1:-}" in
  get)   path=/git-credential ;;
  erase) path=/git-credential/erase ;;
  *)     exit 0 ;;
esac
out=$(curl -sS --unix-socket "${HOTBED_GIT_CRED_SOCK:-/run/hotbed/git-auth/git-cred.sock}" -w '\n%{http_code}' --data-binary @- "http://localhost$path") || exit 1
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

// StartGitAuthBridge exposes a local Unix socket at sockPath inside the guest
// (e.g. /run/hotbed/git-auth/git-cred.sock) and forwards every connection to
// dialHost (vsock CID 2, GitAuthPort on Firecracker).
func StartGitAuthBridge(ctx context.Context, sockPath string, dialHost func() (net.Conn, error)) error {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(dir, "git-credential-helper"), []byte(guestGitCredentialHelper), 0o755)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0o666)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func(local net.Conn) {
			defer local.Close()
			remote, err := dialHost()
			if err != nil {
				return
			}
			defer remote.Close()
			bridgeConns(local, remote)
		}(c)
	}
}

func bridgeConns(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	return 1
}

var diffHunkRe = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

func parseChangedLines(diff string, n int) []int32 {
	delta := make([]int, n+2)
	for line := range strings.SplitSeq(diff, "\n") {
		m := diffHunkRe.FindStringSubmatch(line)
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

func countLines(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}
