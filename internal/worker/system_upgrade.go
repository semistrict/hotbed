package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	defaultCladefsdBin     = "/usr/local/bin/cladefsd"
	defaultCladefsdService = "cladefsd.service"
	defaultFirecrackerBin  = "/usr/local/bin/firecracker"
)

// SystemUpgradeOps overrides host OS actions during Engine.UpgradeSystem (used
// in unit and CLI tests; nil fields use production defaults).
type SystemUpgradeOps struct {
	InstallFile    func(src, dst, wantSHA string, mode os.FileMode) error
	RestartService func(ctx context.Context, service string) error
	Reconnect      func(ctx context.Context) (uint64, error)
}

func (e *Engine) setSystemUpgrading(v bool) {
	e.mu.Lock()
	e.systemUpgrading = v
	e.mu.Unlock()
	if !v {
		e.stepSoon()
	}
}

func (e *Engine) isSystemUpgrading() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.systemUpgrading
}

// UpgradeSystem executes an explicit, disruptive host cladefs/system runtime
// upgrade on this worker:
//  1. Stage and SHA-256-verify all configured artifacts BEFORE disrupting any
//     running work.
//  2. If nothing changed and !req.Force, return "up-to-date" without disruption.
//     If req.DryRun, return "dry-run" with counts of active work that would be
//     terminated.
//  3. Drain admission, terminate all live commands and shells, release all
//     active cladefs/VM workspaces (syncing/stopping VMs and unmounting host
//     FUSE cells), atomically install updated binaries, restart cladefsd,
//     reconnect cladefsd (+ re-clear FUSE nosuid), and resume admission.
func (e *Engine) UpgradeSystem(ctx context.Context, req *pb.WorkerUpgradeSystemRequest) (*pb.NodeSystemUpgrade, error) {
	if req == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "upgrade request is required")
	}

	if e.cfg.VM != nil && e.cfg.VM.OnMachined() {
		return nil, api.Errorf(api.CodePlatformNotSupported, "this worker's VMs and cells are machined's, which hotbed's daemon upgrades with machined itself: there is no cladefsd or Firecracker to upgrade")
	}

	e.upgradeMu.Lock()
	defer e.upgradeMu.Unlock()

	arch := goruntime.GOARCH
	res := &pb.NodeSystemUpgrade{
		Node: e.cfg.Host,
		Arch: arch,
	}

	cladefsdBin := cmp.Or(strings.TrimSpace(req.GetCladefsdBin()), defaultCladefsdBin)
	cladefsdService := cmp.Or(strings.TrimSpace(req.GetCladefsdService()), defaultCladefsdService)
	firecrackerBin := cmp.Or(strings.TrimSpace(req.GetFirecrackerBin()), defaultFirecrackerBin)

	artifacts := e.cfg.Artifacts
	if artifacts == nil && e.cfg.Dir != "" {
		artifacts = &ArtifactStore{WorkerDir: e.cfg.Dir}
	}

	// Phase 1: Stage and SHA-256-verify all artifacts before disrupting work.
	var (
		stagedCladefsd    string
		cladefsdChanged   bool
		stagedFirecracker string
		fcChanged         bool
		stagedKernel      string
		kernelChanged     bool
	)

	if art := req.GetCladefsd(); art != nil && strings.TrimSpace(art.GetSha256()) != "" {
		if artifacts == nil {
			return nil, api.Errorf(api.CodeInternal, "worker has no artifact store configured")
		}
		p, err := artifacts.Ensure(ctx, artifactCladefsd, arch, art)
		if err != nil {
			return nil, fmt.Errorf("stage cladefsd (%s): %w", arch, err)
		}
		stagedCladefsd = p
		res.CladefsdVersion = art.GetVersion()
		res.CladefsdSha256 = strings.ToLower(strings.TrimSpace(art.GetSha256()))
		ok, _ := fileMatchesSHA256(cladefsdBin, res.CladefsdSha256)
		cladefsdChanged = !ok
	}

	hasFirecrackerTarget := e.cfg.VM != nil
	if !hasFirecrackerTarget {
		if _, err := os.Stat(firecrackerBin); err == nil {
			hasFirecrackerTarget = true
		}
	}
	if art := req.GetFirecracker(); art != nil && strings.TrimSpace(art.GetSha256()) != "" && hasFirecrackerTarget {
		if artifacts == nil {
			return nil, api.Errorf(api.CodeInternal, "worker has no artifact store configured")
		}
		p, err := artifacts.Ensure(ctx, artifactFirecracker, arch, art)
		if err != nil {
			return nil, fmt.Errorf("stage firecracker (%s): %w", arch, err)
		}
		stagedFirecracker = p
		res.FirecrackerVersion = art.GetVersion()
		res.FirecrackerSha256 = strings.ToLower(strings.TrimSpace(art.GetSha256()))
		ok, _ := fileMatchesSHA256(firecrackerBin, res.FirecrackerSha256)
		fcChanged = !ok
	}

	if art := req.GetKernel(); art != nil && strings.TrimSpace(art.GetSha256()) != "" && e.cfg.VM != nil {
		if artifacts == nil {
			return nil, api.Errorf(api.CodeInternal, "worker has no artifact store configured")
		}
		p, err := artifacts.Ensure(ctx, artifactKernel, arch, art)
		if err != nil {
			return nil, fmt.Errorf("stage guest kernel (%s): %w", arch, err)
		}
		stagedKernel = p
		res.KernelVersion = art.GetVersion()
		res.KernelSha256 = strings.ToLower(strings.TrimSpace(art.GetSha256()))
		curKernel := e.cfg.VM.CurrentKernelPath()
		ok, _ := fileMatchesSHA256(curKernel, res.KernelSha256)
		kernelChanged = !ok || curKernel != stagedKernel
	}

	hasCladefsBackend := e.cfg.VM != nil || e.cfg.CladefsFUSE != nil
	needRestart := (cladefsdChanged || req.GetForce()) && (hasCladefsBackend || (e.cfg.SystemUpgradeOps != nil && e.cfg.SystemUpgradeOps.RestartService != nil))
	needAnyChange := cladefsdChanged || fcChanged || kernelChanged || needRestart

	if !needAnyChange {
		res.Status = "up-to-date"
		return res, nil
	}

	if req.GetDryRun() {
		cmds, shells, wsIDs := e.snapshotActiveUpgradeTargets()
		res.Status = "dry-run"
		res.CladefsdUpdated = cladefsdChanged
		res.FirecrackerUpdated = fcChanged
		res.KernelUpdated = kernelChanged
		res.StoppedCommands = int32(len(cmds))
		res.ClosedShells = int32(len(shells))
		res.ReleasedWorkspaces = int32(len(wsIDs))
		res.RestartedCladefsd = needRestart
		return res, nil
	}

	// Phase 2: Drain admission and terminate all active commands, shells, and
	// cladefs/VM workspaces so cladefsd can restart cleanly with zero dangling
	// VMs or FUSE mounts.
	slog.Info("starting explicit cladefs system upgrade",
		"node", e.cfg.Host, "arch", arch,
		"cladefsdChanged", cladefsdChanged, "firecrackerChanged", fcChanged,
		"kernelChanged", kernelChanged, "force", req.GetForce(),
	)
	e.setSystemUpgrading(true)
	defer e.setSystemUpgrading(false)

	cmds, shells, wsIDs := e.snapshotActiveUpgradeTargets()
	for _, cid := range cmds {
		if _, err := e.Stop(ctx, cid, "cladefs system upgrade"); err != nil {
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != api.CodeNotFound {
				slog.Warn("stop command during cladefs system upgrade", "command", cid, "err", err)
			}
		} else {
			res.StoppedCommands++
		}
	}
	for _, sh := range shells {
		sh.cancel()
	}
	for _, sh := range shells {
		select {
		case <-sh.done:
			res.ClosedShells++
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for _, wsID := range wsIDs {
		if err := e.ReleaseWorkspace(ctx, wsID); err != nil {
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != api.CodeNotFound {
				slog.Warn("release workspace during cladefs system upgrade", "workspace", wsID, "err", err)
			}
		} else {
			res.ReleasedWorkspaces++
		}
	}

	// Stop any remaining VMs (e.g., base provisioning VMs) and detach remaining
	// FUSE mounts + close cladefsd gRPC clients before restarting cladefsd.
	if e.cfg.VM != nil {
		if err := e.cfg.VM.StopAllForUpgrade(ctx); err != nil {
			slog.Warn("stop VM backend before cladefs upgrade", "err", err)
		}
	}
	if e.cfg.CladefsFUSE != nil {
		if err := e.cfg.CladefsFUSE.StopAllForUpgrade(ctx); err != nil {
			slog.Warn("stop cladefs FUSE backend before cladefs upgrade", "err", err)
		}
	}

	// Phase 3: Atomically install updated binaries, restart cladefsd, and
	// reconnect backends.
	installFn := syncFileAtomically
	if e.cfg.SystemUpgradeOps != nil && e.cfg.SystemUpgradeOps.InstallFile != nil {
		installFn = e.cfg.SystemUpgradeOps.InstallFile
	}
	restartFn := restartSystemdService
	if e.cfg.SystemUpgradeOps != nil && e.cfg.SystemUpgradeOps.RestartService != nil {
		restartFn = e.cfg.SystemUpgradeOps.RestartService
	}

	if cladefsdChanged {
		if err := installFn(stagedCladefsd, cladefsdBin, res.CladefsdSha256, 0o755); err != nil {
			return nil, fmt.Errorf("install cladefsd to %s: %w", cladefsdBin, err)
		}
		res.CladefsdUpdated = true
	}
	if fcChanged {
		if err := installFn(stagedFirecracker, firecrackerBin, res.FirecrackerSha256, 0o755); err != nil {
			return nil, fmt.Errorf("install firecracker to %s: %w", firecrackerBin, err)
		}
		res.FirecrackerUpdated = true
	}
	if kernelChanged && e.cfg.VM != nil {
		e.cfg.VM.SetKernelPath(stagedKernel)
		res.KernelUpdated = true
	}

	if needRestart {
		if err := restartFn(ctx, cladefsdService); err != nil {
			return nil, fmt.Errorf("restart %s: %w", cladefsdService, err)
		}
		res.RestartedCladefsd = true

		if e.cfg.SystemUpgradeOps != nil && e.cfg.SystemUpgradeOps.Reconnect != nil {
			inc, err := e.cfg.SystemUpgradeOps.Reconnect(ctx)
			if err != nil {
				return nil, fmt.Errorf("reconnect cladefsd after restart: %w", err)
			}
			res.CladefsIncarnation = inc
		} else {
			if e.cfg.VM != nil {
				inc, err := e.cfg.VM.ReconnectCladefs(ctx)
				if err != nil {
					return nil, fmt.Errorf("reconnect VM cladefsd after restart: %w", err)
				}
				res.CladefsIncarnation = inc
			}
			if e.cfg.CladefsFUSE != nil {
				inc, err := e.cfg.CladefsFUSE.ReconnectCladefs(ctx)
				if err != nil {
					return nil, fmt.Errorf("reconnect host-FUSE cladefsd after restart: %w", err)
				}
				res.CladefsIncarnation = inc
			}
		}
	}

	res.Status = "upgraded"
	slog.Info("completed explicit cladefs system upgrade",
		"node", e.cfg.Host,
		"cladefsdUpdated", res.CladefsdUpdated,
		"firecrackerUpdated", res.FirecrackerUpdated,
		"kernelUpdated", res.KernelUpdated,
		"restartedCladefsd", res.RestartedCladefsd,
		"incarnation", res.CladefsIncarnation,
		"stoppedCommands", res.StoppedCommands,
		"releasedWorkspaces", res.ReleasedWorkspaces,
	)
	return res, nil
}

func (e *Engine) snapshotActiveUpgradeTargets() (cmdIDs []string, shells []*activeShell, wsIDs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, c := range e.commands {
		if api.Live(c.cmd().State) {
			cmdIDs = append(cmdIDs, id)
		}
	}
	sort.Strings(cmdIDs)
	for _, sh := range e.shells {
		shells = append(shells, sh)
	}
	for id, w := range e.workspaces {
		st := w.ws().State
		if st == released || w.releasing {
			continue
		}
		rt := w.ws().Runtime
		if rt == "vm" || rt == "cladefs" || e.usesRuntime(id) {
			wsIDs = append(wsIDs, id)
		}
	}
	sort.Strings(wsIDs)
	return cmdIDs, shells, wsIDs
}

func restartSystemdService(ctx context.Context, service string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForCladefsReconnect polls cladefs.Open until control.sock answers and
// check(store) succeeds, or until timeout expires.
func waitForCladefsReconnect(ctx context.Context, cfg cladefs.Config, timeout time.Duration, check func(context.Context, *cladefs.Store) (uint64, error)) (*cladefs.Store, uint64, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := cladefs.Open(cctx, cfg)
		if err == nil {
			inc, cerr := check(cctx, st)
			cancel()
			if cerr == nil {
				return st, inc, nil
			}
			_ = st.Close()
			lastErr = cerr
		} else {
			cancel()
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, 0, fmt.Errorf("timed out waiting for cladefsd after %s (last err: %w)", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
