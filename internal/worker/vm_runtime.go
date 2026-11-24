package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/runtime"
	"github.com/semistrict/hotbed/internal/unit"
)

// isVMWorkspace reports whether workspace wsID was created with runtime="vm".
// It reads the lock-free vmWorkspaces index so it is safe to call both with and
// without e.mu held (e.g. from endBy/step/Exec as well as runtimeUnits).
func (e *Engine) isVMWorkspace(wsID string) bool {
	_, ok := e.vmWorkspaces.Load(wsID)
	return ok
}

// resolveWorkspaceRuntime picks "vm", "cladefs", or "chroot" for a CreateWorkspaceRequest.
func (e *Engine) resolveWorkspaceRuntime(req *pb.CreateWorkspaceRequest) (string, error) {
	rt := req.GetRuntime()
	if rt == "" && req.GetPlacement() != nil {
		rt = req.GetPlacement().GetRuntime()
	}
	if req.GetPlacement().GetBench() {
		if rt == runtime.KindVM {
			return "", api.Errorf(api.CodeInvalidArgs, "--bench requires chroot runtime, not vm")
		}
		return runtime.KindChroot, nil
	}
	if rt == "" {
		if e.cfg.VM != nil {
			return e.cfg.VM.DefaultRuntime(), nil
		}
		if e.cfg.CladefsFUSE != nil {
			return e.cfg.CladefsFUSE.DefaultRuntime(), nil
		}
		if e.cfg.Runtime != nil && e.cfg.RuntimeFor == nil {
			return e.cfg.Runtime.Kind(), nil
		}
		return runtime.KindChroot, nil
	}
	switch rt {
	case runtime.KindChroot:
		return runtime.KindChroot, nil
	case runtime.KindVM:
		if e.cfg.VM == nil && e.cfg.Runtime == nil {
			return "", api.Errorf(api.CodePlatformNotSupported, "vm runtime is not enabled on this worker")
		}
		return runtime.KindVM, nil
	case runtime.KindCladefs:
		if e.cfg.CladefsFUSE == nil {
			return "", api.Errorf(api.CodePlatformNotSupported, "cladefs host-FUSE runtime is not enabled on this worker")
		}
		return runtime.KindCladefs, nil
	default:
		return "", api.Errorf(api.CodeInvalidArgs, "unknown runtime %q (want vm, cladefs, or chroot)", rt)
	}
}

// applyCladefsHeartbeat forwards controller-provided cladefs peer addresses
// and retired host incarnations to the local cladefsd backend.
func (e *Engine) applyCladefsHeartbeat(ctx context.Context, reply *pb.HeartbeatResponse) {
	if reply == nil {
		return
	}
	if e.cfg.VM != nil {
		e.cfg.VM.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations())
	} else if e.cfg.CladefsFUSE != nil {
		e.cfg.CladefsFUSE.ApplyHeartbeat(ctx, reply.GetCladefsPeers(), reply.GetDecommissionIncarnations())
	}
}

// usesRuntime reports whether workspace wsID is executed via cfg.Runtime
// (e.g. a Firecracker microVM via hotbed-guestd) instead of host chroot.
func (e *Engine) usesRuntime(wsID string) bool {
	if e.cfg.Runtime == nil || wsID == "" {
		return false
	}
	if e.cfg.RuntimeFor != nil {
		return e.cfg.RuntimeFor(wsID)
	}
	return true
}

// vmCgroupPrefix marks synthetic cgroup identifiers for commands running inside
// a guest VM so Memory() and Reclaim() route through WorkspaceRuntime instead
// of reading or reclaiming host /sys/fs/cgroup files.
const vmCgroupPrefix = "vm:"

func makeVMCgroup(wsID, cmdID string) string {
	return vmCgroupPrefix + wsID + ":" + cmdID
}

func parseVMCgroup(cg string) (wsID, cmdID string, ok bool) {
	rest, ok := strings.CutPrefix(cg, vmCgroupPrefix)
	if !ok {
		return "", "", false
	}
	wsID, cmdID, ok = strings.Cut(rest, ":")
	return wsID, cmdID, ok && wsID != "" && cmdID != ""
}

// runtimeUnits wraps a host Units implementation (Systemd) and routes commands
// and interactive shells belonging to VM-backed workspaces through
// runtime.WorkspaceRuntime (hotbed-guestd over AF_VSOCK), while leaving chroot
// workspaces, base freshens, and --bench commands on the inner host Units.
type runtimeUnits struct {
	inner Units
	rt    runtime.WorkspaceRuntime
	useRT func(wsID string) bool
	uid   int
	gid   int

	mu   sync.Mutex
	cmds map[string]*rtCmd // unitName -> active/tracked guest command
}

type rtCmd struct {
	wsID    string
	cmdID   string
	logPath string
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRuntimeUnits(inner Units, rt runtime.WorkspaceRuntime, useRT func(string) bool, uid, gid int) *runtimeUnits {
	return &runtimeUnits{
		inner: inner,
		rt:    rt,
		useRT: useRT,
		uid:   uid,
		gid:   gid,
		cmds:  map[string]*rtCmd{},
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}

// RegisterExisting re-attaches a live VM command loaded from worker.db after a
// hotbed-worker restart and resumes streaming its guest logs into logPath from
// the existing host file offset.
func (u *runtimeUnits) RegisterExisting(ctx context.Context, unitName, wsID, cmdID, logPath string) {
	if !u.useRT(wsID) {
		return
	}
	u.mu.Lock()
	if _, exists := u.cmds[unitName]; exists {
		u.mu.Unlock()
		return
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: logPath,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	u.cmds[unitName] = rc
	u.mu.Unlock()

	go u.pumpLogs(pumpCtx, rc)
}

func (u *runtimeUnits) lookup(unitName string) *rtCmd {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cmds[unitName]
}

func (u *runtimeUnits) Start(ctx context.Context, s unit.Spec) error {
	wsID := envValue(s.Env, "HOTBED_WORKSPACE")
	if wsID == "" || !u.useRT(wsID) {
		return u.inner.Start(ctx, s)
	}
	cmdID := envValue(s.Env, "HOTBED_COMMAND")
	if cmdID == "" {
		cmdID = strings.TrimPrefix(s.Name, "hotbed-")
	}
	if s.Log != "" {
		if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
			return err
		}
	}
	env := append([]string(nil), s.Env...)
	if len(s.PathPrefix) > 0 {
		env = append(env, "PATH="+strings.Join(s.PathPrefix, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	err := u.rt.StartCommand(ctx, wsID, runtime.CommandSpec{
		ID:         cmdID,
		Argv:       s.Argv,
		Dir:        s.Dir,
		Env:        env,
		User:       s.User,
		UID:        u.uid,
		GID:        u.gid,
		CPUWeight:  s.CPUWeight,
		MemoryLow:  s.MemoryLow,
		MemoryHigh: s.MemoryHigh,
		LogPath:    s.Log,
	})
	if err != nil {
		return err
	}

	pumpCtx, cancel := context.WithCancel(context.Background())
	rc := &rtCmd{
		wsID:    wsID,
		cmdID:   cmdID,
		logPath: s.Log,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	u.mu.Lock()
	u.cmds[s.Name] = rc
	u.mu.Unlock()

	go u.pumpLogs(pumpCtx, rc)
	return nil
}

func (u *runtimeUnits) pumpLogs(ctx context.Context, rc *rtCmd) {
	defer close(rc.done)
	if rc.logPath == "" {
		_, _ = u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, 0, true, io.Discard)
		return
	}
	f, err := os.OpenFile(rc.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("open host log for VM command", "command", rc.cmdID, "path", rc.logPath, "err", err)
		return
	}
	defer f.Close()

	var offset int64
	if st, err := f.Stat(); err == nil {
		offset = st.Size()
	}
	for {
		n, err := u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, true, f)
		offset += n
		if err == nil || ctx.Err() != nil {
			return
		}
		// Check whether the command already exited before retrying.
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stMap, serr := u.rt.StatusMany(sctx, rc.wsID, []string{rc.cmdID})
		cancel()
		if serr == nil {
			if st, ok := stMap[rc.cmdID]; !ok || st.Exited {
				// Final non-follow drain to ensure trailing bytes are flushed.
				_, _ = u.rt.StreamLogs(ctx, rc.wsID, rc.cmdID, offset, false, f)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func toUnitState(wsID string, st runtime.CommandStatus) unit.State {
	return unit.State{
		Loaded:   true,
		Running:  st.Running || st.Frozen,
		Exited:   st.Exited,
		ExitCode: st.ExitCode,
		Memory:   st.MemoryAnon,
		CPUNanos: uint64(max(st.CPUUsec, 0)) * 1000,
		Cgroup:   makeVMCgroup(wsID, st.ID),
	}
}

func (u *runtimeUnits) Status(ctx context.Context, name string) (unit.State, error) {
	rc := u.lookup(name)
	if rc == nil {
		return u.inner.Status(ctx, name)
	}
	m, err := u.rt.StatusMany(ctx, rc.wsID, []string{rc.cmdID})
	if err != nil {
		return unit.State{}, err
	}
	st, ok := m[rc.cmdID]
	if !ok {
		return unit.State{Loaded: false}, nil
	}
	return toUnitState(rc.wsID, st), nil
}

func (u *runtimeUnits) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	var hostNames []string
	byWS := map[string][]*rtCmd{}
	byUnit := map[string]*rtCmd{}
	for _, name := range names {
		if rc := u.lookup(name); rc != nil {
			byWS[rc.wsID] = append(byWS[rc.wsID], rc)
			byUnit[name] = rc
		} else {
			hostNames = append(hostNames, name)
		}
	}
	out := make(map[string]unit.State, len(names))
	if len(hostNames) > 0 {
		hostStates, err := u.inner.StatusMany(ctx, hostNames)
		if err != nil {
			return nil, err
		}
		for k, v := range hostStates {
			out[k] = v
		}
	}
	for wsID, rcs := range byWS {
		ids := make([]string, len(rcs))
		for i, rc := range rcs {
			ids[i] = rc.cmdID
		}
		m, err := u.rt.StatusMany(ctx, wsID, ids)
		if err != nil {
			return nil, err
		}
		for name, rc := range byUnit {
			if rc.wsID != wsID {
				continue
			}
			if st, ok := m[rc.cmdID]; ok {
				out[name] = toUnitState(wsID, st)
			} else {
				out[name] = unit.State{Loaded: false}
			}
		}
	}
	return out, nil
}

// Freeze freezes the command's cgroup v2 INSIDE the guest via hotbed-guestd.
// Never freezes the host Firecracker process (which would trigger cladefsd
// quarantine timeout / exit 158).
func (u *runtimeUnits) Freeze(ctx context.Context, name string) error {
	if rc := u.lookup(name); rc != nil {
		return u.rt.FreezeCommand(ctx, rc.wsID, rc.cmdID)
	}
	return u.inner.Freeze(ctx, name)
}

func (u *runtimeUnits) Thaw(ctx context.Context, name string) error {
	if rc := u.lookup(name); rc != nil {
		return u.rt.ThawCommand(ctx, rc.wsID, rc.cmdID)
	}
	return u.inner.Thaw(ctx, name)
}

func (u *runtimeUnits) Reclaim(cgroup string, n int64) error {
	if _, _, ok := parseVMCgroup(cgroup); ok {
		// Guest cgroups cannot be reclaimed via host memory.reclaim; never touch
		// the host Firecracker VMM cgroup directly.
		return nil
	}
	return u.inner.Reclaim(cgroup, n)
}

func (u *runtimeUnits) Memory(cgroup string) (unit.MemoryUse, error) {
	if wsID, cmdID, ok := parseVMCgroup(cgroup); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m, err := u.rt.StatusMany(ctx, wsID, []string{cmdID})
		if err != nil {
			return unit.MemoryUse{}, err
		}
		st, found := m[cmdID]
		if !found {
			return unit.MemoryUse{}, os.ErrNotExist
		}
		return unit.MemoryUse{Anon: st.MemoryAnon, Swap: st.MemorySwap}, nil
	}
	return u.inner.Memory(cgroup)
}

func (u *runtimeUnits) Stop(ctx context.Context, name string) error {
	rc := u.lookup(name)
	if rc == nil {
		return u.inner.Stop(ctx, name)
	}
	err := u.rt.StopCommand(ctx, rc.wsID, rc.cmdID, 3*time.Second)
	// Wait briefly for the log pump to flush final output before returning.
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		rc.cancel()
		<-rc.done
	case <-ctx.Done():
		rc.cancel()
	}
	u.mu.Lock()
	delete(u.cmds, name)
	u.mu.Unlock()
	return err
}

func (u *runtimeUnits) List(ctx context.Context, slice string) ([]string, error) {
	hostList, err := u.inner.List(ctx, slice)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	for name := range u.cmds {
		hostList = append(hostList, name)
	}
	u.mu.Unlock()
	return hostList, nil
}

func (u *runtimeUnits) Wait(ctx context.Context, slice, name string) error {
	if rc := u.lookup(name); rc != nil {
		select {
		case <-rc.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return u.inner.Wait(ctx, slice, name)
}

func (u *runtimeUnits) StartShell(ctx context.Context, s unit.Spec, sio unit.ShellIO) (unit.ShellHandle, error) {
	wsID := envValue(s.Env, "HOTBED_WORKSPACE")
	if wsID == "" || !u.useRT(wsID) {
		return u.inner.StartShell(ctx, s, sio)
	}
	env := append([]string(nil), s.Env...)
	if len(s.PathPrefix) > 0 {
		env = append(env, "PATH="+strings.Join(s.PathPrefix, ":")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return u.rt.StartShell(ctx, wsID, runtime.ShellSpec{
		Argv: s.Argv,
		Dir:  s.Dir,
		Env:  env,
		User: s.User,
		UID:  u.uid,
		GID:  u.gid,
	}, sio)
}

func (u *runtimeUnits) Exec(ctx context.Context, name, slice string, c unit.ExecSpec) (*exec.Cmd, error) {
	return u.inner.Exec(ctx, name, slice, c)
}

// applyViaRuntime delegates git bundle application inside a VM workspace to
// hotbed-guestd over AF_VSOCK (multipart /v1/sync), without touching host paths.
func (e *Engine) applyViaRuntime(ctx context.Context, wsID, repo, bundlePath, commit, baseCommit string, subs []gitops.SubCheckout) (int, error) {
	var uid, gid int
	if e.cred != nil {
		uid, gid = int(e.cred.Uid), int(e.cred.Gid)
	} else if e.cfg.BaseImage != nil {
		sp := e.cfg.BaseImage.Spec.Normalized()
		uid, gid = sp.UID, sp.GID
	}
	return e.cfg.Runtime.ApplySync(ctx, wsID, runtime.SyncSpec{
		Repo:       repo,
		BundlePath: bundlePath,
		Commit:     commit,
		BaseCommit: baseCommit,
		Subs:       subs,
		Env:        e.sandboxEnv(),
		UID:        uid,
		GID:        gid,
	})
}

func (e *Engine) filesViaRuntime(ctx context.Context, wsID string, repos []*pb.WorkspaceRepo) ([]*pb.FileEntry, error) {
	refs := make([]runtime.RepoRef, len(repos))
	for i, r := range repos {
		refs[i] = runtime.RepoRef{Repo: r.Repo, BaseCommit: r.BaseCommit}
	}
	return e.cfg.Runtime.ListFiles(ctx, wsID, refs)
}

func (e *Engine) fileViaRuntime(ctx context.Context, wsID, path string, repos []*pb.WorkspaceRepo) (*pb.FileContent, error) {
	clean := strings.TrimPrefix(filepath.Clean("/"+path), "/")
	name, rel, _ := strings.Cut(clean, "/")
	var r *pb.WorkspaceRepo
	for _, o := range repos {
		if o.Repo == name {
			r = o
		}
	}
	if r == nil || rel == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "path %q is in none of the workspace's repos", path).
			WithSuggestion("paths start with the repo, such as hotbed/go.mod; hotbed workspace files lists them")
	}
	fc, err := e.cfg.Runtime.GetFile(ctx, wsID, r.Repo, r.BaseCommit, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, api.Errorf(api.CodeNotFound, "%s: %v", path, err)
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "%s: %v", path, err)
	}
	fc.Path = path
	return fc, nil
}

// portProxy forwards TCP connections on a host port to a dev server listening
// on loopback inside a VM workspace via WorkspaceRuntime.DialPort (vsock 5003).
type portProxy struct {
	ln   net.Listener
	port uint16
	wsID string
	rt   runtime.WorkspaceRuntime
}

func startPortProxy(listenAddr string, wsID string, guestPort uint16, rt runtime.WorkspaceRuntime) (*portProxy, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen dev server proxy %s -> %s:%d: %w", listenAddr, wsID, guestPort, err)
	}
	p := &portProxy{ln: ln, port: guestPort, wsID: wsID, rt: rt}
	go p.serve()
	return p, nil
}

func (p *portProxy) Addr() string { return p.ln.Addr().String() }

func (p *portProxy) serve() {
	for {
		src, err := p.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			dst, err := p.rt.DialPort(ctx, p.wsID, p.port)
			cancel()
			if err != nil {
				return
			}
			defer dst.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(dst, c)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(c, dst)
				if cw, ok := c.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}()
			<-done
			<-done
		}(src)
	}
}

func (p *portProxy) Close() error { return p.ln.Close() }
