package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/guestd"
)

// SuspendedState captures everything needed to resume a Firecracker microVM
// from its CladeFS memory cell and snapshot state file, even across a
// hotbed-worker restart.
type SuspendedState struct {
	ID                string             `json:"id"`
	UnitName          string             `json:"unitName"`
	Dir               string             `json:"dir"`
	CellPath          string             `json:"cellPath"`
	CladefsVMSock     string             `json:"cladefsVMSock"`
	SnapshotStatePath string             `json:"snapshotStatePath"`
	MemoryCellPath    string             `json:"memoryCellPath"`
	PrevMemCellPaths  []string           `json:"prevMemCellPaths,omitempty"`
	MemoryCell        MemoryCellSnapshot `json:"memoryCell"`
	VsockUDS          string             `json:"vsockUDS"`
	ConsoleLog        string             `json:"consoleLog"`
	GuestCID          uint32             `json:"guestCID"`
	Net               NetAlloc           `json:"net"`
	WorkingSetBytes   int64              `json:"workingSetBytes,omitempty"`
	SuspendedAt       time.Time          `json:"suspendedAt"`
	ToolsDiskPath     string             `json:"toolsDiskPath,omitempty"`
	VCPUs             int                `json:"vcpus,omitempty"`
	MemMiB            int                `json:"memMiB,omitempty"`
}

func (s *SuspendedState) clone() *SuspendedState {
	if s == nil {
		return nil
	}
	cp := *s
	cp.PrevMemCellPaths = slices.Clone(s.PrevMemCellPaths)
	return &cp
}

func (s *SuspendedState) allMemoryCellPaths() []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, p := range s.PrevMemCellPaths {
		add(p)
	}
	add(s.MemoryCellPath)
	return out
}

func (i *Instance) allMemoryCellPaths() []string {
	if i == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, p := range i.PrevMemCellPaths {
		add(p)
	}
	add(i.MemoryCellPath)
	return out
}

// IsSuspended reports whether id currently has a suspended VM snapshot backed
// by a CladeFS memory cell.
func (m *Manager) IsSuspended(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.suspended[id]
	return ok
}

// Suspended returns a copy of id's SuspendedState, or nil if id is not
// suspended.
func (m *Manager) Suspended(id string) *SuspendedState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspended[id].clone()
}

// ListSuspended returns the IDs of all currently suspended VMs.
func (m *Manager) ListSuspended() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.suspended))
	for id := range m.suspended {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// MemoryCellPaths returns all CladeFS memory cell host paths associated with
// VM id (whether currently live or suspended) so callers can delete them once
// the VM is permanently discarded.
func (m *Manager) MemoryCellPaths(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst := m.instances[id]; inst != nil {
		return inst.allMemoryCellPaths()
	}
	if susp := m.suspended[id]; susp != nil {
		return susp.allMemoryCellPaths()
	}
	return nil
}

// Suspend freezes the guest filesystem (FIFREEZE), pauses Firecracker, writes
// guest RAM straight into a CladeFS memory cell (PUT /snapshot/create with
// memory_cell), persists suspended metadata, and stops the Firecracker VMM so
// the VM holds zero host RAM and zero swap while parked or idle.
//
// For a cold-booted VM, memCellHostPath is the new memory cell directory on the
// host (inside the tenant's cell root). For a VM already restored from a memory
// cell, Firecracker writes only dirty private pages into its already-open
// forked memory cell and memCellHostPath is ignored.
func (m *Manager) Suspend(ctx context.Context, id, memCellHostPath string, workingSetBytes int64) (*SuspendedState, error) {
	if id == "" {
		return nil, errors.New("vm suspend: id is required")
	}
	lock := m.vmLock(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if existing := m.suspended[id]; existing != nil {
		if workingSetBytes > 0 && existing.WorkingSetBytes == 0 {
			existing.WorkingSetBytes = workingSetBytes
		}
		cp := existing.clone()
		m.mu.Unlock()
		return cp, nil
	}
	inst := m.instances[id]
	m.mu.Unlock()
	if inst == nil {
		return nil, fmt.Errorf("vm %s: not running", id)
	}
	if inst.APISock == "" {
		return nil, fmt.Errorf("vm %s has no Firecracker API socket: %w", id, ErrSuspendUnsupported)
	}
	if inst.Client == nil {
		return nil, fmt.Errorf("vm %s: no guestd client", id)
	}
	if !inst.FromMemoryCell && memCellHostPath == "" {
		return nil, fmt.Errorf("vm %s: memCellHostPath is required for initial suspend", id)
	}

	// 1. Drop clean guest page cache and freeze the cladefs rootfs inside the
	// guest so the disk snapshot version is completely consistent.
	if err := inst.Client.FreezeFS(ctx, true); err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil, fmt.Errorf("vm %s guestd lacks FreezeFS: %w", id, ErrSuspendUnsupported)
		}
		return nil, fmt.Errorf("vm %s: freeze guest fs: %w", id, err)
	}

	thawGuestFS := func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := inst.Client.ThawFS(tctx); err != nil {
			slog.Warn("vm suspend rollback: thaw guest fs failed", "vm", id, "err", err)
		}
	}

	// 2. Pause Firecracker vCPUs via PATCH /vm.
	fcAPI := newFCAPIClient(inst.APISock)
	if err := fcAPI.PatchVM(ctx, "Paused"); err != nil {
		thawGuestFS()
		return nil, fmt.Errorf("vm %s: pause firecracker: %w", id, err)
	}

	resumeAndThaw := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fcAPI.PatchVM(rctx, "Resumed"); err != nil {
			slog.Warn("vm suspend rollback: resume firecracker failed", "vm", id, "err", err)
		}
		thawGuestFS()
	}

	// 3. Snapshot microVM state + guest memory straight into a CladeFS memory cell.
	snapStatePath := filepath.Join(inst.Dir, "snapshot.state")
	_ = os.Remove(snapStatePath)
	var memCfg *CreateMemoryCellConfig
	var activeMemCellPath string
	if inst.FromMemoryCell {
		// VM was already restored from a memory cell: omit path fields so
		// Firecracker commits only dirty private pages into its existing cell.
		memCfg = &CreateMemoryCellConfig{}
		activeMemCellPath = inst.MemoryCellPath
	} else {
		// Cold-booted workspace VM: create a standalone host-local ephemeral
		// memory cell (zero bucket uploads).
		memCfg = &CreateMemoryCellConfig{
			PathOnHost:     memCellHostPath,
			CladefsdSocket: inst.CladefsVMSock,
			Ephemeral:      true,
		}
		activeMemCellPath = memCellHostPath
	}
	memSnap, err := fcAPI.CreateSnapshot(ctx, CreateSnapshotRequest{
		SnapshotType: "Full",
		SnapshotPath: snapStatePath,
		MemoryCell:   memCfg,
	})
	if err != nil {
		_ = os.Remove(snapStatePath)
		resumeAndThaw()
		return nil, fmt.Errorf("vm %s: create memory-cell snapshot: %w", id, err)
	}

	susp := &SuspendedState{
		ID:                inst.ID,
		UnitName:          inst.UnitName,
		Dir:               inst.Dir,
		CellPath:          inst.CellPath,
		CladefsVMSock:     inst.CladefsVMSock,
		SnapshotStatePath: snapStatePath,
		MemoryCellPath:    activeMemCellPath,
		PrevMemCellPaths:  slices.Clone(inst.PrevMemCellPaths),
		MemoryCell:        memSnap,
		VsockUDS:          inst.VsockUDS,
		ConsoleLog:        inst.ConsoleLog,
		GuestCID:          inst.GuestCID,
		Net:               inst.Net,
		WorkingSetBytes:   workingSetBytes,
		SuspendedAt:       time.Now().UTC(),
		ToolsDiskPath:     inst.ToolsDiskPath,
		VCPUs:             inst.VCPUs,
		MemMiB:            inst.MemMiB,
	}

	// 4. Persist suspended metadata BEFORE stopping Firecracker so a worker crash
	// mid-stop recovers cleanly into the suspended state (never losing the snapshot).
	meta := vmMeta{
		ID:               inst.ID,
		UnitName:         inst.UnitName,
		CellPath:         inst.CellPath,
		CladefsVMSock:    inst.CladefsVMSock,
		APISock:          inst.APISock,
		VsockUDS:         inst.VsockUDS,
		ConsoleLog:       inst.ConsoleLog,
		GuestCID:         inst.GuestCID,
		Net:              inst.Net,
		StartedAt:        inst.StartedAt,
		FromMemoryCell:   true,
		MemoryCellPath:   activeMemCellPath,
		PrevMemCellPaths: slices.Clone(inst.PrevMemCellPaths),
		ToolsDiskPath:    inst.ToolsDiskPath,
		VCPUs:            inst.VCPUs,
		MemMiB:           inst.MemMiB,
		Suspended:        susp,
	}
	if err := writeMeta(inst.Dir, meta); err != nil {
		_ = os.Remove(snapStatePath)
		resumeAndThaw()
		return nil, fmt.Errorf("vm %s: persist suspended metadata: %w", id, err)
	}

	// 5. Terminate the paused Firecracker process immediately without resuming it
	// (nothing must write to the rootfs cell after the snapshot cut). Keep GuestCID
	// and Net.GuestIP reserved while tearing down the host TAP device.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.stopTimeout())
	defer cancel()
	if err := m.forceStopUnit(stopCtx, inst.UnitName); err != nil {
		slog.Error("vm suspend: stop paused firecracker unit failed; preserving suspended metadata",
			"vm", id, "unit", inst.UnitName, "err", err)
		return nil, fmt.Errorf("vm %s: stop suspended firecracker unit: %w", id, err)
	}
	if inst.gitAuth != nil {
		_ = inst.gitAuth.Close()
	}
	if m.net != nil && inst.Net.TAPName != "" {
		m.net.ReleaseTAPKeepIP(stopCtx, inst.Net)
	}
	_ = os.Remove(inst.APISock)
	_ = os.Remove(inst.VsockUDS)

	m.mu.Lock()
	if cur := m.instances[id]; cur == inst {
		delete(m.instances, id)
	}
	m.suspended[id] = susp
	m.mu.Unlock()

	slog.Info("suspended Firecracker VM to cladefs memory cell",
		"vm", id, "tenant", memSnap.Tenant, "cell", memSnap.Cell, "version", memSnap.Version,
		"workingSetBytes", workingSetBytes)
	return susp.clone(), nil
}

// Resume spawns a fresh Firecracker VMM, restores the suspended VM via
// PUT /snapshot/load with MemBackendType::Cell (lazy userfaultfd demand paging
// from the CladeFS memory cell), waits for hotbed-guestd to answer over vsock,
// and thaws the guest filesystem (FITHAW).
//
// For host-local ephemeral memory cells (susp.MemoryCell.Ephemeral == true),
// cladefsd reopens susp.MemoryCellPath in place with zero bucket I/O and
// nextMemCellHostPath may be empty. For bucket-backed memory cell versions,
// nextMemCellHostPath is the fresh cell directory path into which cladefsd
// forks the snapshot's memory cell version for this resumed run.
func (m *Manager) Resume(ctx context.Context, id, nextMemCellHostPath string) (*Instance, error) {
	if id == "" {
		return nil, errors.New("vm resume: id is required")
	}
	lock := m.vmLock(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if existing := m.instances[id]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	susp := m.suspended[id].clone()
	cfg := m.cfg
	m.mu.Unlock()
	if susp == nil {
		return nil, fmt.Errorf("vm %s: not suspended", id)
	}
	backendPath := nextMemCellHostPath
	if susp.MemoryCell.Ephemeral {
		backendPath = susp.MemoryCellPath
	}
	if backendPath == "" {
		return nil, fmt.Errorf("vm %s: memory cell backend path is required to resume", id)
	}

	if err := os.MkdirAll(susp.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("vm %s: ensure dir %s: %w", id, susp.Dir, err)
	}
	_ = chownSockForUser(susp.Dir, cfg.VMUser)
	_ = os.Chmod(susp.Dir, 0o755)

	apiSock := filepath.Join(susp.Dir, "fc.sock")
	_ = os.Remove(apiSock)
	_ = os.Remove(susp.VsockUDS)

	if m.net != nil && susp.Net.TAPName != "" {
		if err := m.net.RecreateTAP(ctx, id, susp.Net); err != nil {
			return nil, fmt.Errorf("vm %s: recreate tap %s: %w", id, susp.Net.TAPName, err)
		}
	}

	var gaBridge *gitAuthBridge
	if cfg.GitAuthHostSock != "" {
		var err error
		gaBridge, err = startGitAuthBridge(susp.VsockUDS, cfg.GitAuthHostSock, cfg.VMUser)
		if err != nil {
			if m.net != nil && susp.Net.TAPName != "" {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				m.net.ReleaseTAPKeepIP(rctx, susp.Net)
				cancel()
			}
			return nil, fmt.Errorf("vm %s: restart git-auth bridge: %w", id, err)
		}
	}

	rollbackResume := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), m.stopTimeout())
		_ = m.forceStopUnit(stopCtx, susp.UnitName)
		if gaBridge != nil {
			_ = gaBridge.Close()
		}
		if m.net != nil && susp.Net.TAPName != "" {
			m.net.ReleaseTAPKeepIP(stopCtx, susp.Net)
		}
		cancel()
		_ = os.Remove(apiSock)
		_ = os.Remove(susp.VsockUDS)
	}

	// Ensure Firecracker can append to console.log when running as VMUser.
	if f, err := os.OpenFile(susp.ConsoleLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_ = f.Close()
		_ = chownSockForUser(susp.ConsoleLog, cfg.VMUser)
	}

	procSpec := ProcessSpec{
		Name:       susp.UnitName,
		Slice:      cfg.Slice,
		User:       cfg.VMUser,
		Binary:     cfg.FirecrackerBin,
		Args:       []string{"--enable-pci", "--api-sock", apiSock},
		ConsoleLog: susp.ConsoleLog,
	}
	if err := m.launcher.Start(ctx, procSpec); err != nil {
		rollbackResume()
		return nil, fmt.Errorf("vm %s: launch firecracker for resume: %w", id, err)
	}

	checkExited := func() error {
		if st, serr := m.launcher.Status(ctx, susp.UnitName); serr == nil && st.Loaded && st.Exited {
			tail := readTail(susp.ConsoleLog, 2048)
			if tail != "" {
				return fmt.Errorf("%s\nconsole tail:\n%s", ExitReason(st.ExitCode), tail)
			}
			return errors.New(ExitReason(st.ExitCode))
		}
		return nil
	}
	if err := waitAPISock(ctx, apiSock, cfg.BootTimeout, checkExited); err != nil {
		rollbackResume()
		return nil, fmt.Errorf("vm %s: wait for firecracker api socket: %w", id, err)
	}

	loadReq := LoadSnapshotRequest{
		SnapshotPath: susp.SnapshotStatePath,
		MemBackend: MemBackendConfig{
			BackendType: "Cell",
			BackendPath: backendPath,
		},
		MemoryCell: &MemoryCellConfig{
			Tenant:         susp.MemoryCell.Tenant,
			Cell:           susp.MemoryCell.Cell,
			Version:        susp.MemoryCell.Version,
			CladefsdSocket: susp.CladefsVMSock,
			Ephemeral:      susp.MemoryCell.Ephemeral,
		},
		ResumeVM: true,
		VsockOverride: &VsockOverride{
			UDSPath: susp.VsockUDS,
		},
		CladefsOverrides: []CladefsOverride{{
			ID:             "rootfs",
			PathOnHost:     susp.CellPath,
			CladefsdSocket: susp.CladefsVMSock,
		}},
	}
	if susp.Net.TAPName != "" {
		loadReq.NetworkOverrides = []NetworkOverride{{
			IfaceID:     "eth0",
			HostDevName: susp.Net.TAPName,
		}}
	}

	fcAPI := newFCAPIClient(apiSock)
	if err := fcAPI.LoadSnapshot(ctx, loadReq); err != nil {
		rollbackResume()
		return nil, fmt.Errorf("vm %s: load memory-cell snapshot: %w", id, err)
	}

	client := guestd.NewClient(m.dialerFn(susp.VsockUDS))
	if err := m.waitHealthy(ctx, susp.UnitName, susp.ConsoleLog, client); err != nil {
		rollbackResume()
		return nil, fmt.Errorf("vm %s: wait guestd after resume: %w", id, err)
	}
	if err := client.ThawFS(ctx); err != nil {
		rollbackResume()
		return nil, fmt.Errorf("vm %s: thaw guest fs after resume: %w", id, err)
	}

	prevMem := slices.Clone(susp.PrevMemCellPaths)
	if susp.MemoryCellPath != "" && susp.MemoryCellPath != backendPath && !slices.Contains(prevMem, susp.MemoryCellPath) {
		prevMem = append(prevMem, susp.MemoryCellPath)
	}

	now := time.Now().UTC()
	inst := &Instance{
		ID:               susp.ID,
		UnitName:         susp.UnitName,
		Dir:              susp.Dir,
		CellPath:         susp.CellPath,
		CladefsVMSock:    susp.CladefsVMSock,
		APISock:          apiSock,
		VsockUDS:         susp.VsockUDS,
		ConsoleLog:       susp.ConsoleLog,
		GuestCID:         susp.GuestCID,
		Net:              susp.Net,
		Client:           client,
		StartedAt:        now,
		FromMemoryCell:   true,
		MemoryCellPath:   backendPath,
		PrevMemCellPaths: prevMem,
		ToolsDiskPath:    susp.ToolsDiskPath,
		VCPUs:            susp.VCPUs,
		MemMiB:           susp.MemMiB,
		gitAuth:          gaBridge,
	}

	meta := vmMeta{
		ID:               inst.ID,
		UnitName:         inst.UnitName,
		CellPath:         inst.CellPath,
		CladefsVMSock:    inst.CladefsVMSock,
		APISock:          inst.APISock,
		VsockUDS:         inst.VsockUDS,
		ConsoleLog:       inst.ConsoleLog,
		GuestCID:         inst.GuestCID,
		Net:              inst.Net,
		StartedAt:        inst.StartedAt,
		FromMemoryCell:   true,
		MemoryCellPath:   inst.MemoryCellPath,
		PrevMemCellPaths: slices.Clone(inst.PrevMemCellPaths),
		ToolsDiskPath:    inst.ToolsDiskPath,
		VCPUs:            inst.VCPUs,
		MemMiB:           inst.MemMiB,
	}
	if err := writeMeta(inst.Dir, meta); err != nil {
		slog.Warn("vm resume: update metadata failed", "vm", id, "err", err)
	}
	_ = os.Remove(susp.SnapshotStatePath)

	m.mu.Lock()
	delete(m.suspended, id)
	m.instances[id] = inst
	m.mu.Unlock()

	slog.Info("resumed Firecracker VM from cladefs memory cell",
		"vm", id, "memoryCell", backendPath, "ephemeral", susp.MemoryCell.Ephemeral,
		"sourceCell", susp.MemoryCell.Cell, "sourceVersion", susp.MemoryCell.Version)
	return inst, nil
}
