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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
	"github.com/semistrict/hotbed/internal/gen/guestd/v1/guestdv1connect"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/inspect"
	"github.com/semistrict/hotbed/internal/profiler"
	"github.com/semistrict/hotbed/internal/pty"
)

const (
	maxFileBytes       = 1 << 20
	maxSyncBundleBytes = 2 << 30
	maxControlMsgBytes = 16 << 20
)

var (
	validCmdID      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	errSyncTooLarge = errors.New("sync bundle exceeds size limit")
)

var _ guestdv1connect.GuestServiceHandler = (*Server)(nil)

// ServerConfig configures a guestd.Server.
type ServerConfig struct {
	WorkDir   string            // workspace root containing repo checkouts (default /work)
	StateDir  string            // runtime dir for logs and temp sync bundles (default /run/hotbed-guestd)
	RunDir    string            // base directory for per-user XDG_RUNTIME_DIR (<RunDir>/user/<uid>, default /run)
	Logger    *slog.Logger
	Inspector inspect.Inspector // defaults to inspect.Linux{}
	Profiler  profiler.Profiler // optional always-on in-guest eBPF continuous profiler
	// AllowPowerOff permits Shutdown(PowerOff=true) to reboot/power off the VM.
	// Must only be enabled by the in-VM hotbed-guestd daemon (never in-process
	// host tests running as root, which would otherwise reboot the host).
	AllowPowerOff bool
	PowerOff      func() error // optional override for powerOffGuest (tests)
}

// Server runs inside a workspace VM and handles requests from hotbed-worker.
type Server struct {
	workDir       string
	stateDir      string
	runDir        string
	logDir        string
	syncDir       string
	log           *slog.Logger
	inspector     inspect.Inspector
	profiler      profiler.Profiler
	allowPowerOff bool
	powerOff      func() error

	mu   sync.Mutex
	cmds map[string]*managedCmd
}

type managedCmd struct {
	mu           sync.Mutex
	id           string
	cmd          *exec.Cmd
	cg           *cmdCgroup
	logPath      string
	logFile      *os.File
	state        guestdv1.CommandState
	exit         int
	signal       int
	oomKilled    bool
	result       string
	peakMem      int64
	peakSwap     int64
	finalCPUUsec int64
	mainPID      int
	started      int64
	exited       int64
	errMsg       string
	done         chan struct{}
}

// NewServer constructs a guestd Server and prepares its state directories.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/work"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/run/hotbed-guestd"
	}
	if cfg.RunDir == "" {
		cfg.RunDir = "/run"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	insp := cfg.Inspector
	if insp == nil {
		insp = inspect.Linux{}
	}
	logDir := filepath.Join(cfg.StateDir, "logs")
	syncDir := filepath.Join(cfg.StateDir, "sync")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guestd log dir: %w", err)
	}
	if err := os.MkdirAll(syncDir, 0o755); err != nil {
		return nil, fmt.Errorf("create guestd sync dir: %w", err)
	}
	powerOffFn := cfg.PowerOff
	if powerOffFn == nil {
		powerOffFn = powerOffGuest
	}
	return &Server{
		workDir:       cfg.WorkDir,
		stateDir:      cfg.StateDir,
		runDir:        cfg.RunDir,
		logDir:        logDir,
		syncDir:       syncDir,
		log:           cfg.Logger,
		inspector:     insp,
		profiler:      cfg.Profiler,
		allowPowerOff: cfg.AllowPowerOff,
		powerOff:      powerOffFn,
		cmds:          map[string]*managedCmd{},
	}, nil
}

// guestErrorInterceptor converts *api.Error values into Connect errors carrying
// hotbed.v1.ErrorInfo details so structured error codes and suggestions survive
// unary and streaming RPC boundaries.
func guestErrorInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err == nil {
			return nil
		}
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce
		}
		var ae *api.Error
		if errors.As(err, &ae) {
			return api.ToConnect(ae)
		}
		return err
	}
}

// Handler returns the Connect HTTP handler for ControlPort.
func (s *Server) Handler() http.Handler {
	cs := connect.NewServer(guestErrorInterceptor)
	guestdv1connect.RegisterGuestServiceHandler(cs, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, cs, connecthttp.WithReadMaxBytes(maxControlMsgBytes))
	return mux
}

// ServeControl serves the Connect control API on ln until ctx is cancelled.
func (s *Server) ServeControl(ctx context.Context, ln net.Listener) error {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           s.Handler(),
		Protocols:         protocols,
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

func (s *Server) Health(_ context.Context, _ *guestdv1.HealthRequest) (*guestdv1.HealthResponse, error) {
	host, _ := os.Hostname()
	return &guestdv1.HealthResponse{
		Hostname: host,
		Kernel:   kernelRelease(),
		Arch:     runtime.GOARCH,
		Pid1:     os.Getpid() == 1,
		CgroupV2: hasCgroupV2(),
		WorkDir:  s.workDir,
	}, nil
}

// ensureRuntimeDir creates <runDir>/user/<uid> (0700, owned by uid:gid when
// running as root) so rootless podman and other XDG-aware tools have a valid
// XDG_RUNTIME_DIR inside minimal Firecracker VMs without systemd-logind.
func (s *Server) ensureRuntimeDir(uid, gid int) string {
	if uid <= 0 || s.runDir == "" {
		return ""
	}
	userRoot := filepath.Join(s.runDir, "user")
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		s.log.Warn("create runtime user root failed", "path", userRoot, "err", err)
		return ""
	}
	_ = os.Chmod(userRoot, 0o755)

	dir := filepath.Join(userRoot, strconv.Itoa(uid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Warn("create runtime user dir failed", "path", dir, "err", err)
		return ""
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, uid, gid); err != nil {
			s.log.Warn("chown runtime user dir failed", "path", dir, "uid", uid, "gid", gid, "err", err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		s.log.Warn("chmod runtime user dir failed", "path", dir, "err", err)
	}
	return dir
}

// withDefaultRuntimeEnv returns env with XDG_RUNTIME_DIR=runtimeDir set when
// runtimeDir is non-empty and env does not already specify XDG_RUNTIME_DIR.
func withDefaultRuntimeEnv(env []string, runtimeDir string) []string {
	if runtimeDir == "" {
		return env
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			return env
		}
	}
	if len(env) == 0 {
		base := os.Environ()
		out := make([]string, 0, len(base)+1)
		for _, kv := range base {
			if !strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
				out = append(out, kv)
			}
		}
		return append(out, "XDG_RUNTIME_DIR="+runtimeDir)
	}
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	return append(out, "XDG_RUNTIME_DIR="+runtimeDir)
}

func (s *Server) StartCommand(_ context.Context, req *guestdv1.StartCommandRequest) (*guestdv1.CommandStatus, error) {
	if !validCmdID.MatchString(req.GetId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid command id %q", req.GetId()))
	}
	if len(req.GetArgv()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "argv is required")
	}

	// Idempotent start: if ID already exists, return its current status.
	s.mu.Lock()
	if existing, ok := s.cmds[req.Id]; ok {
		s.mu.Unlock()
		return existing.snapshot(), nil
	}

	logPath := filepath.Join(s.logDir, req.Id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeInternal, "open log file: "+err.Error())
	}

	dir := s.resolveWorkDir(req.Dir)
	runtimeDir := s.ensureRuntimeDir(int(req.Uid), int(req.Gid))
	env := withDefaultRuntimeEnv(req.Env, runtimeDir)
	cmd, err := commandWithEnv(req.Argv, env)
	if err != nil {
		_ = logFile.Close()
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeInternal, "start command: "+err.Error())
	}
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	attr := &syscall.SysProcAttr{Setpgid: true}
	if req.Uid > 0 || req.Gid > 0 {
		if os.Geteuid() == 0 {
			attr.Credential = &syscall.Credential{
				Uid: uint32(req.Uid),
				Gid: uint32(req.Gid),
			}
		}
	}
	cmd.SysProcAttr = attr

	cg := createCmdCgroup(req.Id, req)
	usedCgroupFD, closeCgroupFD := attachCgroupAttr(attr, cg)
	err = cmd.Start()
	closeCgroupFD()
	if err != nil {
		_ = logFile.Close()
		if cg != nil {
			cg.remove()
		}
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeInternal, "start command: "+err.Error())
	}
	pid := cmd.Process.Pid
	if cg != nil && !usedCgroupFD {
		_ = cg.addPID(pid)
	}
	setCmdOOMScoreAdj(pid, 1000)

	mc := &managedCmd{
		id:      req.Id,
		cmd:     cmd,
		cg:      cg,
		logPath: logPath,
		logFile: logFile,
		state:   guestdv1.CommandState_COMMAND_STATE_RUNNING,
		mainPID: pid,
		started: time.Now().UnixMilli(),
		done:    make(chan struct{}),
	}
	s.cmds[req.Id] = mc
	s.mu.Unlock()

	go s.waitCommand(mc)

	return mc.snapshot(), nil
}

func (s *Server) waitCommand(mc *managedCmd) {
	err := mc.cmd.Wait()
	code, sig := exitStatusOf(err)
	mc.mu.Lock()
	var finalStats cmdCgroupStats
	if mc.cg != nil {
		// Read final memory.events (oom_kill), memory.peak, memory.swap.peak,
		// and cpu.stat BEFORE killing/removing the cgroup directory.
		finalStats = mc.cg.stats()
		mc.cg.killAll()
		mc.cg.remove()
	}
	mc.state = guestdv1.CommandState_COMMAND_STATE_EXITED
	mc.exit = code
	mc.signal = sig
	mc.oomKilled = finalStats.oomKilled
	mc.result = classifyExitResult(code, sig, finalStats.oomKilled)
	mc.peakMem = max(mc.peakMem, finalStats.memPeak, finalStats.memAnon)
	mc.peakSwap = max(mc.peakSwap, finalStats.swapPeak, finalStats.memSwap)
	mc.finalCPUUsec = max(mc.finalCPUUsec, finalStats.cpuUsec)
	mc.exited = time.Now().UnixMilli()
	if err != nil && code == 0 && sig == 0 {
		mc.errMsg = err.Error()
	}
	_ = mc.logFile.Close()
	close(mc.done)
	mc.mu.Unlock()
}

func (mc *managedCmd) snapshot() *guestdv1.CommandStatus {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	var logBytes int64
	if st, err := os.Stat(mc.logPath); err == nil {
		logBytes = st.Size()
	}
	var cgPath string
	var memAnon, memSwap, memPeak, swapPeak, cpuUsec int64
	if mc.cg != nil {
		cgPath = mc.cg.path
		if mc.state != guestdv1.CommandState_COMMAND_STATE_EXITED {
			st := mc.cg.stats()
			memAnon, memSwap, cpuUsec = st.memAnon, st.memSwap, st.cpuUsec
			mc.peakMem = max(mc.peakMem, st.memPeak, st.memAnon)
			mc.peakSwap = max(mc.peakSwap, st.swapPeak, st.memSwap)
			mc.finalCPUUsec = max(mc.finalCPUUsec, st.cpuUsec)
			memPeak, swapPeak = mc.peakMem, mc.peakSwap
		} else {
			memPeak = mc.peakMem
			swapPeak = mc.peakSwap
			cpuUsec = mc.finalCPUUsec
		}
	}
	return &guestdv1.CommandStatus{
		Id:             mc.id,
		State:          mc.state,
		ExitCode:       int32(mc.exit),
		Signal:         int32(mc.signal),
		OomKilled:      mc.oomKilled,
		Result:         mc.result,
		MainPid:        int32(mc.mainPID),
		Cgroup:         cgPath,
		MemoryAnon:     memAnon,
		MemorySwap:     memSwap,
		MemoryPeak:     memPeak,
		MemorySwapPeak: swapPeak,
		CpuUsec:        cpuUsec,
		LogBytes:       logBytes,
		StartedMs:      mc.started,
		ExitedMs:       mc.exited,
		Error:          mc.errMsg,
	}
}

func (s *Server) Status(_ context.Context, req *guestdv1.StatusRequest) (*guestdv1.StatusResponse, error) {
	s.mu.Lock()
	var list []*managedCmd
	if len(req.GetIds()) == 0 {
		for _, mc := range s.cmds {
			list = append(list, mc)
		}
	} else {
		for _, id := range req.GetIds() {
			if mc, ok := s.cmds[id]; ok {
				list = append(list, mc)
			}
		}
	}
	s.mu.Unlock()

	out := make(map[string]*guestdv1.CommandStatus, len(list))
	for _, mc := range list {
		out[mc.id] = mc.snapshot()
	}
	return &guestdv1.StatusResponse{Commands: out}, nil
}

func (s *Server) getCmd(id string) (*managedCmd, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mc, ok := s.cmds[id]
	return mc, ok
}

func (s *Server) StopCommand(_ context.Context, req *guestdv1.StopCommandRequest) (*guestdv1.CommandStatus, error) {
	mc, ok := s.getCmd(req.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "unknown command "+req.GetId())
	}
	grace := time.Duration(req.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = 3 * time.Second
	}
	s.stopManagedCmd(mc, grace)
	return mc.snapshot(), nil
}

func (s *Server) stopManagedCmd(mc *managedCmd, grace time.Duration) {
	mc.mu.Lock()
	if mc.state == guestdv1.CommandState_COMMAND_STATE_EXITED {
		mc.mu.Unlock()
		return
	}
	pid := mc.mainPID
	cg := mc.cg
	wasFrozen := mc.state == guestdv1.CommandState_COMMAND_STATE_FROZEN
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

func (s *Server) FreezeCommand(_ context.Context, req *guestdv1.FreezeCommandRequest) (*guestdv1.CommandStatus, error) {
	mc, ok := s.getCmd(req.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "unknown command "+req.GetId())
	}
	mc.mu.Lock()
	if mc.state == guestdv1.CommandState_COMMAND_STATE_EXITED {
		mc.mu.Unlock()
		return mc.snapshot(), nil
	}
	var err error
	if mc.cg != nil {
		err = mc.cg.freeze()
	} else {
		err = signalProcessGroup(mc.mainPID, syscall.SIGSTOP)
	}
	if err == nil {
		mc.state = guestdv1.CommandState_COMMAND_STATE_FROZEN
	}
	mc.mu.Unlock()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "freeze command: "+err.Error())
	}
	return mc.snapshot(), nil
}

func (s *Server) ThawCommand(_ context.Context, req *guestdv1.ThawCommandRequest) (*guestdv1.CommandStatus, error) {
	mc, ok := s.getCmd(req.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "unknown command "+req.GetId())
	}
	mc.mu.Lock()
	if mc.state == guestdv1.CommandState_COMMAND_STATE_EXITED {
		mc.mu.Unlock()
		return mc.snapshot(), nil
	}
	var err error
	if mc.cg != nil {
		err = mc.cg.thaw()
	} else {
		err = signalProcessGroup(mc.mainPID, syscall.SIGCONT)
	}
	if err == nil {
		mc.state = guestdv1.CommandState_COMMAND_STATE_RUNNING
	}
	mc.mu.Unlock()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "thaw command: "+err.Error())
	}
	return mc.snapshot(), nil
}

func (s *Server) Reclaim(_ context.Context, req *guestdv1.ReclaimRequest) (*guestdv1.ReclaimResponse, error) {
	// Drop clean guest page caches first so virtio-balloon free-page-reporting
	// returns clean file cache immediately and swapless cgroup reclaim sees only
	// remaining reclaimable file/slab bytes instead of spinning on anon RAM.
	_ = dropPageCaches()
	if req.GetId() != "" && req.GetBytes() > 0 {
		if mc, ok := s.getCmd(req.GetId()); ok && mc.cg != nil {
			_ = mc.cg.reclaim(req.GetBytes())
		}
	}
	return &guestdv1.ReclaimResponse{}, nil
}

func (s *Server) inspectTarget(id string) (inspect.Target, error) {
	if id == "" {
		return inspect.Target{}, api.Errorf(api.CodeInvalidArgs, "command id is required")
	}
	mc, ok := s.getCmd(id)
	if !ok {
		return inspect.Target{}, api.Errorf(api.CodeNotFound, "command %s not found in guest VM", id)
	}
	mc.mu.Lock()
	state := mc.state
	mainPID := mc.mainPID
	var cgPath string
	if mc.cg != nil {
		cgPath = strings.TrimPrefix(mc.cg.path, "/sys/fs/cgroup")
	}
	mc.mu.Unlock()

	if state == guestdv1.CommandState_COMMAND_STATE_EXITED {
		return inspect.Target{}, api.Errorf(api.CodeConflict, "command %s has already ended; live inspection only applies while running", id)
	}
	host, _ := os.Hostname()
	t := inspect.Target{
		CommandID: id,
		UnitName:  "hotbed-" + id,
		Cgroup:    cgPath,
		MainPID:   mainPID,
		Hostname:  host,
		Kernel:    kernelRelease(),
		Arch:      runtime.GOARCH,
	}
	if s.profiler != nil {
		t.ProfileWindow = func(fromMs, toMs int64) *pb.CommandProfile {
			return s.profiler.Query(id, &pb.GetProfileRequest{
				Target: &pb.GetProfileRequest_CommandId{CommandId: id},
				FromMs: fromMs,
				ToMs:   toMs,
				Limit:  100,
			}, true)
		}
	}
	return t, nil
}

func (s *Server) InspectCommand(ctx context.Context, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	target, err := s.inspectTarget(req.GetId())
	if err != nil {
		return nil, err
	}
	return s.inspector.Inspect(ctx, target, req)
}

func (s *Server) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, stream guestdv1connect.GuestServiceTraceCommandServerStream) error {
	target, err := s.inspectTarget(req.GetId())
	if err != nil {
		return err
	}
	var sendMu sync.Mutex
	return s.inspector.Trace(ctx, target, req, func(ch *pb.TraceCommandChunk) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ch)
	})
}

func (s *Server) SampleCommand(ctx context.Context, req *pb.SampleCommandRequest, stream guestdv1connect.GuestServiceSampleCommandServerStream) error {
	target, err := s.inspectTarget(req.GetId())
	if err != nil {
		return err
	}
	var sendMu sync.Mutex
	return s.inspector.Sample(ctx, target, req, func(ch *pb.SampleCommandChunk) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ch)
	})
}

func (s *Server) GetProfile(_ context.Context, req *pb.GetProfileRequest) (*pb.CommandProfile, error) {
	if node := req.GetNode(); node != "" {
		if s.profiler == nil {
			return &pb.CommandProfile{
				Node:       node,
				Profiler:   profiler.EngineName,
				Live:       true,
				Active:     false,
				StatusNote: "continuous profiler not configured in guest VM",
			}, nil
		}
		return s.profiler.QueryNode(node, req), nil
	}
	cmdID := req.GetCommandId()
	if cmdID == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "command_id or node is required")
	}
	mc, ok := s.getCmd(cmdID)
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "command %s not found in guest VM", cmdID)
	}
	mc.mu.Lock()
	isLive := mc.state != guestdv1.CommandState_COMMAND_STATE_EXITED
	mc.mu.Unlock()

	if s.profiler == nil {
		return &pb.CommandProfile{
			CommandId:  cmdID,
			Profiler:   profiler.EngineName,
			Live:       isLive,
			Active:     false,
			StatusNote: "continuous profiler not configured in guest VM",
		}, nil
	}
	return s.profiler.Query(cmdID, req, isLive), nil
}

func (s *Server) ForgetCommandProfile(_ context.Context, req *guestdv1.ForgetCommandProfileRequest) (*guestdv1.ForgetCommandProfileResponse, error) {
	if s.profiler != nil && req.GetId() != "" {
		s.profiler.Forget(req.GetId())
	}
	return &guestdv1.ForgetCommandProfileResponse{}, nil
}

func (s *Server) StreamLogs(ctx context.Context, req *guestdv1.StreamLogsRequest, stream guestdv1connect.GuestServiceStreamLogsServerStream) error {
	mc, ok := s.getCmd(req.GetId())
	if !ok {
		return connect.NewError(connect.CodeNotFound, "unknown command "+req.GetId())
	}
	offset := req.GetOffset()
	if offset < 0 {
		offset = 0
	}
	follow := req.GetFollow()

	f, err := os.Open(mc.logPath)
	if err != nil {
		return connect.NewError(connect.CodeInternal, "open log: "+err.Error())
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, "seek log: "+err.Error())
		}
	}

	buf := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&guestdv1.StreamLogsResponse{Data: bytes.Clone(buf[:n])}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			if !follow {
				return nil
			}
			select {
			case <-mc.done:
				// Drain any final bytes written before close.
				for {
					n, err := f.Read(buf)
					if n > 0 {
						if serr := stream.Send(&guestdv1.StreamLogsResponse{Data: bytes.Clone(buf[:n])}); serr != nil {
							return serr
						}
					}
					if err != nil {
						return nil
					}
				}
			case <-ctx.Done():
				return nil
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if rerr != nil {
			return connect.NewError(connect.CodeInternal, "read log: "+rerr.Error())
		}
	}
}

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
		entries, err := listRepoFiles(ctx, repoRoot, dir, repo.BaseCommit)
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

	st, err := repoRoot.Lstat(req.RelPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, connect.NewError(connect.CodeNotFound, err.Error())
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if !st.Mode().IsRegular() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("%s is not a regular file", req.RelPath))
	}
	f, err := repoRoot.Open(req.RelPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}

	var changed []int32
	if req.BaseCommit != "" {
		g := gitops.Git{Dir: filepath.Join(s.workDir, req.Repo)}
		diff, err := g.Run(ctx, "-c", "safe.directory=*", "diff", "-U0", req.BaseCommit, "--", req.RelPath)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, "git diff: "+err.Error())
		}
		changed = parseChangedLines(diff, countLines(data))
	}
	return &pb.FileContent{
		Path:         req.Repo + "/" + req.RelPath,
		Text:         string(data),
		ChangedLines: changed,
	}, nil
}

func (s *Server) Shutdown(_ context.Context, req *guestdv1.ShutdownRequest) (*guestdv1.ShutdownResponse, error) {
	if req.GetStopCommands() {
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
	if req.GetSyncFs() {
		syncFilesystems()
	}
	if req.GetPowerOff() && s.allowPowerOff {
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = s.powerOff()
		}()
	}
	return &guestdv1.ShutdownResponse{}, nil
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

	runtimeDir := s.ensureRuntimeDir(req.UID, req.GID)
	env := append([]string(nil), req.Env...)
	if req.Term != "" {
		env = append(env, "TERM="+req.Term)
	}
	env = withDefaultRuntimeEnv(env, runtimeDir)
	cmd, err := commandWithEnv(argv, env)
	if err != nil {
		_ = send(frameError, []byte("start shell: "+err.Error()))
		return
	}
	cmd.Dir = dir
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
	code, _ := exitStatusOf(err)
	return code
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

// commandWithEnv constructs an *exec.Cmd resolving a bare argv[0] against the
// PATH in env rather than guestd's own PID-1 PATH (which lacks workspace tool
// directories such as /work/.hotbed/bin).
func commandWithEnv(argv, env []string) (*exec.Cmd, error) {
	name := argv[0]
	path := name
	if !strings.Contains(name, "/") {
		resolved, err := lookPathInEnv(name, env)
		if err != nil {
			return nil, err
		}
		path = resolved
	}
	cmd := &exec.Cmd{
		Path: path,
		Args: append([]string(nil), argv...),
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	return cmd, nil
}

func lookPathInEnv(file string, env []string) (string, error) {
	pathVal, hasPath := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVal = v
			hasPath = true
		}
	}
	if !hasPath {
		return exec.LookPath(file)
	}
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		fi, err := os.Stat(candidate)
		if err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}
