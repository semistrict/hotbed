// Package cladefs wraps cladefsd's gRPC control API (cladefs.cladefsd.v1) for
// hotbed workers managing shared-bucket Fxfs cells (base images, project bases,
// and per-workspace forks).
package cladefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	mountv1 "github.com/semistrict/cladefs/go/mount/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/semistrict/hotbed/internal/storage"
)

const (
	DefaultControlSock   = "/run/cladefsd/control.sock"
	DefaultVMSock        = "/run/cladefsd/vm.sock"
	DefaultCapacityBytes = 32 << 30 // 32 GiB sparse Fxfs disk per workspace
)

// Config configures a Store connected to a host's cladefsd instance.
type Config struct {
	ControlSock string // cladefsd gRPC control Unix socket (default /run/cladefsd/control.sock)
	VMSock      string // fallback VM socket when TenantID == 0 (default /run/cladefsd/vm.sock)
	CellRoot    string // host directory containing hotbed's cells (must live on O_DIRECT-capable FS)
	TenantID    uint64 // optional tenant ID; when >0, Store calls Grant to get an isolated tenant VM socket
	VMUID       uint32 // optional UID to own granted tenant socket
	VMGID       uint32 // optional GID to own granted tenant socket
	DefaultCap  uint64 // default cell capacity in bytes (default 32 GiB)
}

// Normalized returns a copy of c with defaults applied.
func (c Config) Normalized() Config {
	if c.ControlSock == "" {
		c.ControlSock = DefaultControlSock
	}
	if c.VMSock == "" {
		c.VMSock = DefaultVMSock
	}
	if c.DefaultCap == 0 {
		c.DefaultCap = DefaultCapacityBytes
	}
	return c
}

// CellRef identifies a cladefs cell (and optionally a specific uploaded version
// that can be forked on any host sharing the bucket).
type CellRef struct {
	RelPath  string `json:"relPath"`
	HostPath string `json:"hostPath"`
	Tenant   uint64 `json:"tenant"`
	Cell     uint64 `json:"cell"`
	Version  uint64 `json:"version,omitempty"`
}

// Store manages hotbed cells through cladefsd's control API.
type Store struct {
	cfg    Config
	api    cladefsdv1.CladefsdClient
	mount  mountv1.MountClient
	health healthpb.HealthClient
	closer io.Closer
	vmSock string
}

// Open connects to cladefsd at cfg.ControlSock, checks health, ensures
// cfg.CellRoot exists, and grants a tenant VM socket if cfg.TenantID > 0.
func Open(ctx context.Context, cfg Config, dialOpts ...grpc.DialOption) (*Store, error) {
	cfg = cfg.Normalized()
	if cfg.CellRoot == "" {
		return nil, errors.New("cladefs: CellRoot is required")
	}
	absRoot, err := filepath.Abs(cfg.CellRoot)
	if err != nil {
		return nil, fmt.Errorf("cladefs: resolve CellRoot: %w", err)
	}
	cfg.CellRoot = absRoot
	if err := os.MkdirAll(cfg.CellRoot, 0o755); err != nil {
		return nil, fmt.Errorf("cladefs: create CellRoot %s: %w", cfg.CellRoot, err)
	}
	client, err := cladefsdv1.NewClient(cfg.ControlSock, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("cladefs: connect %s: %w", cfg.ControlSock, err)
	}
	s, err := NewStore(ctx, cfg, client.CladefsdClient, client.Health, client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	s.mount = client.Mount
	return s, nil
}

// NewStore builds a Store over an already-constructed CladefsdClient (used by
// Open and unit tests).
func NewStore(ctx context.Context, cfg Config, api cladefsdv1.CladefsdClient, health healthpb.HealthClient, closer io.Closer) (*Store, error) {
	cfg = cfg.Normalized()
	if cfg.CellRoot == "" {
		return nil, errors.New("cladefs: CellRoot is required")
	}
	if api == nil {
		return nil, errors.New("cladefs: CladefsdClient is required")
	}
	if health != nil {
		resp, err := health.Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil {
			return nil, fmt.Errorf("cladefs: health check failed: %w", err)
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			return nil, fmt.Errorf("cladefs: daemon not serving (status=%s)", resp.GetStatus())
		}
	}
	vmSock := cfg.VMSock
	if cfg.TenantID > 0 {
		req := &cladefsdv1.GrantRequest{
			Tenant: cfg.TenantID,
			Dir:    cfg.CellRoot,
		}
		if cfg.VMUID > 0 {
			u := cfg.VMUID
			req.Uid = &u
		}
		if cfg.VMGID > 0 {
			g := cfg.VMGID
			req.Gid = &g
		}
		resp, err := api.Grant(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("cladefs: grant tenant %d on %s: %w", cfg.TenantID, cfg.CellRoot, err)
		}
		if resp.GetSocket() == "" {
			return nil, fmt.Errorf("cladefs: grant tenant %d returned empty socket", cfg.TenantID)
		}
		vmSock = resp.GetSocket()
	}
	return &Store{
		cfg:    cfg,
		api:    api,
		health: health,
		closer: closer,
		vmSock: vmSock,
	}, nil
}

// SetMount sets the MountClient used by MountAttach, MountDetach, and MountList
// (used by unit tests constructing a Store via NewStore).
func (s *Store) SetMount(m mountv1.MountClient) {
	s.mount = m
}

// Close closes the underlying gRPC connection.
func (s *Store) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// VMSock returns the VM socket path Firecracker should connect to.
func (s *Store) VMSock() string { return s.vmSock }

// CellRoot returns the host directory containing all managed cells.
func (s *Store) CellRoot() string { return s.cfg.CellRoot }

// TenantID returns the configured tenant ID (0 for default tenant).
func (s *Store) TenantID() uint64 { return s.cfg.TenantID }

// CellPath resolves relPath safely inside s.cfg.CellRoot, rejecting traversal
// or empty/absolute paths.
func (s *Store) CellPath(relPath string) (string, error) {
	if relPath == "" {
		return "", errors.New("cladefs: empty cell path")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("cladefs: cell path %q must be relative", relPath)
	}
	clean := filepath.Clean(relPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cladefs: cell path %q escapes cell root", relPath)
	}
	full := filepath.Join(s.cfg.CellRoot, clean)
	rel, err := filepath.Rel(s.cfg.CellRoot, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cladefs: cell path %q escapes cell root", relPath)
	}
	return full, nil
}

func (s *Store) capacityOrDefault(capBytes uint64) uint64 {
	if capBytes > 0 {
		return capBytes
	}
	return s.cfg.DefaultCap
}

// ensureParentDir creates parent directories of hostPath inside s.cfg.CellRoot
// and matches their ownership to s.cfg.CellRoot so a non-root cladefsd daemon
// can create cell directories beneath them when hotbed-worker runs as root.
func (s *Store) ensureParentDir(hostPath string) error {
	parent := filepath.Dir(hostPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return nil
	}
	rootFi, err := os.Stat(s.cfg.CellRoot)
	if err != nil {
		return nil
	}
	st, ok := rootFi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	// Walk upward from parent to s.cfg.CellRoot and align ownership.
	for cur := parent; cur != s.cfg.CellRoot && strings.HasPrefix(cur, s.cfg.CellRoot+string(filepath.Separator)); cur = filepath.Dir(cur) {
		_ = os.Chown(cur, int(st.Uid), int(st.Gid))
	}
	return nil
}

// ImportImage imports an OCI layout or tar archive at imagePath into a new cell
// at relPath, creates a version, and syncs it to the shared bucket so any host
// can fork it.
func (s *Store) ImportImage(ctx context.Context, relPath, imagePath string, capBytes uint64) (CellRef, error) {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return CellRef{}, err
	}
	if err := s.ensureParentDir(hostPath); err != nil {
		return CellRef{}, fmt.Errorf("cladefs: mkdir parent for %s: %w", relPath, err)
	}
	imp, err := s.api.ImportImage(ctx, &cladefsdv1.ImportImageRequest{
		Path:     hostPath,
		Capacity: s.capacityOrDefault(capBytes),
		Image:    imagePath,
		Tenant:   s.cfg.TenantID,
	})
	if err != nil {
		return CellRef{}, mapErr("import image "+relPath, err)
	}
	ver, err := s.MakeVersionAndSync(ctx, relPath, 0)
	if err != nil {
		return CellRef{}, err
	}
	return CellRef{
		RelPath:  relPath,
		HostPath: hostPath,
		Tenant:   imp.GetTenant(),
		Cell:     imp.GetCell(),
		Version:  ver,
	}, nil
}

// CreateCell creates a new empty Fxfs cell at relPath.
func (s *Store) CreateCell(ctx context.Context, relPath string, capBytes uint64, token *uint64) (CellRef, error) {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return CellRef{}, err
	}
	if err := s.ensureParentDir(hostPath); err != nil {
		return CellRef{}, fmt.Errorf("cladefs: mkdir parent for %s: %w", relPath, err)
	}
	resp, err := s.api.CreateCell(ctx, &cladefsdv1.CreateCellRequest{
		Path:     hostPath,
		Capacity: s.capacityOrDefault(capBytes),
		Token:    token,
		Tenant:   s.cfg.TenantID,
		Kind:     cladefsdv1.CellKind_CELL_KIND_FXFS,
	})
	if err != nil {
		return CellRef{}, mapErr("create cell "+relPath, err)
	}
	return CellRef{
		RelPath:  relPath,
		HostPath: hostPath,
		Tenant:   resp.GetTenant(),
		Cell:     resp.GetCell(),
	}, nil
}

// TakeCell opens an existing bucket-backed cell (tenant, cellID) on this host
// with fencing token, for cross-node workspace moves.
func (s *Store) TakeCell(ctx context.Context, relPath string, tenant, cellID, capBytes uint64, token *uint64) (CellRef, error) {
	if cellID == 0 {
		return CellRef{}, errors.New("cladefs: TakeCell requires non-zero cellID")
	}
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return CellRef{}, err
	}
	if err := s.ensureParentDir(hostPath); err != nil {
		return CellRef{}, fmt.Errorf("cladefs: mkdir parent for %s: %w", relPath, err)
	}
	resp, err := s.api.CreateCell(ctx, &cladefsdv1.CreateCellRequest{
		Path:     hostPath,
		Capacity: s.capacityOrDefault(capBytes),
		Token:    token,
		Tenant:   tenant,
		Cell:     cellID,
		Kind:     cladefsdv1.CellKind_CELL_KIND_FXFS,
	})
	if err != nil {
		return CellRef{}, mapErr("take cell "+relPath, err)
	}
	return CellRef{
		RelPath:  relPath,
		HostPath: hostPath,
		Tenant:   resp.GetTenant(),
		Cell:     resp.GetCell(),
	}, nil
}

// ForkVersion creates a new O(1) copy-on-write cell at toRelPath from an
// uploaded version (which may have been created on any host sharing the bucket).
func (s *Store) ForkVersion(ctx context.Context, from CellRef, toRelPath string, ephemeral bool) (CellRef, error) {
	if from.Cell == 0 || from.Version == 0 {
		return CellRef{}, fmt.Errorf("cladefs: ForkVersion requires cell (%d) and version (%d)", from.Cell, from.Version)
	}
	toHost, err := s.CellPath(toRelPath)
	if err != nil {
		return CellRef{}, err
	}
	if err := s.ensureParentDir(toHost); err != nil {
		return CellRef{}, fmt.Errorf("cladefs: mkdir parent for %s: %w", toRelPath, err)
	}
	resp, err := s.api.ForkCell(ctx, &cladefsdv1.ForkCellRequest{
		To:        toHost,
		Tenant:    from.Tenant,
		Cell:      from.Cell,
		Version:   from.Version,
		Ephemeral: ephemeral,
	})
	if err != nil {
		return CellRef{}, mapErr("fork version into "+toRelPath, err)
	}
	return CellRef{
		RelPath:  toRelPath,
		HostPath: toHost,
		Tenant:   resp.GetTenant(),
		Cell:     resp.GetCell(),
	}, nil
}

// ForkCell forks a local cell at fromRelPath into a new cell at toRelPath.
func (s *Store) ForkCell(ctx context.Context, fromRelPath, toRelPath string) (CellRef, error) {
	fromHost, err := s.CellPath(fromRelPath)
	if err != nil {
		return CellRef{}, err
	}
	toHost, err := s.CellPath(toRelPath)
	if err != nil {
		return CellRef{}, err
	}
	if err := s.ensureParentDir(toHost); err != nil {
		return CellRef{}, fmt.Errorf("cladefs: mkdir parent for %s: %w", toRelPath, err)
	}
	resp, err := s.api.ForkCell(ctx, &cladefsdv1.ForkCellRequest{
		From: fromHost,
		To:   toHost,
	})
	if err != nil {
		return CellRef{}, mapErr("fork cell "+fromRelPath+" -> "+toRelPath, err)
	}
	return CellRef{
		RelPath:  toRelPath,
		HostPath: toHost,
		Tenant:   resp.GetTenant(),
		Cell:     resp.GetCell(),
	}, nil
}

// PlaceCell updates a cell's fencing token so older placements cannot reclaim it.
func (s *Store) PlaceCell(ctx context.Context, relPath string, token uint64) error {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return err
	}
	_, err = s.api.PlaceCell(ctx, &cladefsdv1.PlaceCellRequest{
		Path:  hostPath,
		Token: token,
	})
	return mapErr("place cell "+relPath, err)
}

// MakeVersion creates a named version of relPath on this host.
func (s *Store) MakeVersion(ctx context.Context, relPath string, keepLast uint32) (uint64, error) {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return 0, err
	}
	resp, err := s.api.MakeVersion(ctx, &cladefsdv1.MakeVersionRequest{
		Path:     hostPath,
		KeepLast: keepLast,
	})
	if err != nil {
		return 0, mapErr("make version "+relPath, err)
	}
	return resp.GetVersion(), nil
}

// SyncCell waits until all commits of relPath are uploaded to the shared bucket.
func (s *Store) SyncCell(ctx context.Context, relPath string) error {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return err
	}
	_, err = s.api.SyncCell(ctx, &cladefsdv1.SyncCellRequest{Path: hostPath})
	return mapErr("sync cell "+relPath, err)
}

// MakeVersionAndSync creates a version of relPath and waits until it is uploaded
// to the shared bucket (ready for cross-host ForkVersion).
func (s *Store) MakeVersionAndSync(ctx context.Context, relPath string, keepLast uint32) (uint64, error) {
	ver, err := s.MakeVersion(ctx, relPath, keepLast)
	if err != nil {
		return 0, err
	}
	if err := s.SyncCell(ctx, relPath); err != nil {
		return 0, err
	}
	return ver, nil
}

// StatCell returns cladefsd's metadata and usage stats for relPath.
func (s *Store) StatCell(ctx context.Context, relPath string) (*cladefsdv1.StatCellResponse, error) {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return nil, err
	}
	resp, err := s.api.StatCell(ctx, &cladefsdv1.StatCellRequest{Path: hostPath})
	if err != nil {
		return nil, mapErr("stat cell "+relPath, err)
	}
	return resp, nil
}

// GetCounters returns cladefsd's cumulative daemon and object-store counters.
func (s *Store) GetCounters(ctx context.Context) (*cladefsdv1.GetCountersResponse, error) {
	resp, err := s.api.GetCounters(ctx, &cladefsdv1.GetCountersRequest{})
	if err != nil {
		return nil, mapErr("get counters", err)
	}
	return resp, nil
}

// GetStats returns cladefsd's daemon gauges, counters, and connected VM/guest DAX stats.
func (s *Store) GetStats(ctx context.Context, req *cladefsdv1.GetStatsRequest) (*cladefsdv1.GetStatsResponse, error) {
	if req == nil {
		req = &cladefsdv1.GetStatsRequest{}
	}
	resp, err := s.api.GetStats(ctx, req)
	if err != nil {
		return nil, mapErr("get stats", err)
	}
	return resp, nil
}

// DeleteCell deletes a cell both locally and in the bucket. Deleting an already
// absent cell returns nil (idempotent).
func (s *Store) DeleteCell(ctx context.Context, relPath string) error {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return err
	}
	_, err = s.api.DeleteCell(ctx, &cladefsdv1.DeleteCellRequest{Path: hostPath})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return mapErr("delete cell "+relPath, err)
	}
	return nil
}

// DropCell deletes only this host's local copy of relPath while keeping its
// branch in the shared bucket (call SyncCell first).
func (s *Store) DropCell(ctx context.Context, relPath string) error {
	hostPath, err := s.CellPath(relPath)
	if err != nil {
		return err
	}
	_, err = s.api.DropCell(ctx, &cladefsdv1.DropCellRequest{Path: hostPath})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return mapErr("drop cell "+relPath, err)
	}
	return nil
}

// Base32ID formats a cladefs u64 tenant/cell ID as 13-character zero-padded
// lowercase Crockford base32, matching cladefs's base32_id::Padded layout used
// in CLADEFSD_MOUNT and CLADEFSD_MOUNT_STORES directory names.
func Base32ID(id uint64) string {
	const digits = "0123456789abcdefghjkmnpqrstvwxyz"
	var out [13]byte
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = digits[id%32]
		id /= 32
	}
	return string(out[:])
}

// MountAttach attaches a bucket cell (or creates a new empty Fxfs cell when
// cell == 0) in cladefsd's host FUSE mount (CLADEFSD_MOUNT).
func (s *Store) MountAttach(ctx context.Context, tenant, cell, capBytes uint64, token *uint64) (cellID uint64, shown bool, err error) {
	if s.mount == nil {
		return 0, false, errors.New("cladefs: mount client not available")
	}
	req := &mountv1.AttachRequest{
		Tenant:   tenant,
		Cell:     cell,
		Token:    token,
		Capacity: capBytes,
	}
	if cell == 0 && req.Capacity == 0 {
		req.Capacity = s.cfg.DefaultCap
	}
	resp, err := s.mount.Attach(ctx, req)
	if err != nil {
		return 0, false, mapErr("mount attach", err)
	}
	return resp.GetCell(), resp.GetShown(), nil
}

// MountDetach detaches an attached cell from cladefsd's host FUSE mount.
// When force is false, cladefsd flushes dirty Fxfs state before returning.
func (s *Store) MountDetach(ctx context.Context, tenant, cell uint64, force bool) error {
	if s.mount == nil {
		return errors.New("cladefs: mount client not available")
	}
	_, err := s.mount.Detach(ctx, &mountv1.DetachRequest{
		Tenant: tenant,
		Cell:   cell,
		Force:  force,
	})
	if err != nil && isNotFound(err) {
		return nil
	}
	return mapErr("mount detach", err)
}

// MountList returns all cells currently attached in cladefsd's host FUSE mount.
func (s *Store) MountList(ctx context.Context) ([]*mountv1.AttachedCell, error) {
	if s.mount == nil {
		return nil, errors.New("cladefs: mount client not available")
	}
	resp, err := s.mount.List(ctx, &mountv1.ListRequest{})
	if err != nil {
		return nil, mapErr("mount list", err)
	}
	return resp.GetCells(), nil
}

// SyncCellAbs syncs a cell directory given by absolute host path (such as one
// inside CLADEFSD_MOUNT_STORES).
func (s *Store) SyncCellAbs(ctx context.Context, absPath string) error {
	_, err := s.api.SyncCell(ctx, &cladefsdv1.SyncCellRequest{Path: absPath})
	return mapErr("sync cell "+absPath, err)
}

// MakeVersionAndSyncAbs versions and syncs a cell directory given by absolute
// host path (such as one inside CLADEFSD_MOUNT_STORES).
func (s *Store) MakeVersionAndSyncAbs(ctx context.Context, absPath string, keepLast uint32) (uint64, error) {
	if err := s.SyncCellAbs(ctx, absPath); err != nil {
		return 0, err
	}
	resp, err := s.api.MakeVersion(ctx, &cladefsdv1.MakeVersionRequest{
		Path:     absPath,
		KeepLast: keepLast,
	})
	if err != nil {
		return 0, mapErr("make version "+absPath, err)
	}
	if err := s.SyncCellAbs(ctx, absPath); err != nil {
		return 0, err
	}
	return resp.GetVersion(), nil
}

// DropCellAbs drops only the local directory at absPath while keeping its
// bucket state intact.
func (s *Store) DropCellAbs(ctx context.Context, absPath string) error {
	_, err := s.api.DropCell(ctx, &cladefsdv1.DropCellRequest{Path: absPath})
	if err != nil && isNotFound(err) {
		return nil
	}
	return mapErr("drop cell "+absPath, err)
}

// DeleteCellAbs deletes a cell both locally and in the bucket at absPath.
func (s *Store) DeleteCellAbs(ctx context.Context, absPath string) error {
	_, err := s.api.DeleteCell(ctx, &cladefsdv1.DeleteCellRequest{Path: absPath})
	if err != nil && isNotFound(err) {
		return nil
	}
	return mapErr("delete cell "+absPath, err)
}

// ShrinkMemory asks cladefsd to evict clean extent cache pages down to
// targetBytes (for this tenant if TenantID > 0, else daemon-wide).
func (s *Store) ShrinkMemory(ctx context.Context, targetBytes uint64) (uint64, error) {
	req := &cladefsdv1.ShrinkMemoryRequest{TargetBytes: targetBytes}
	if s.cfg.TenantID > 0 {
		t := s.cfg.TenantID
		req.Tenant = &t
	}
	resp, err := s.api.ShrinkMemory(ctx, req)
	if err != nil {
		return 0, mapErr("shrink memory", err)
	}
	return resp.GetBytes(), nil
}

// SetPeers updates cladefsd's peer list (HOST:PORT addresses) for LAN GetPart.
func (s *Store) SetPeers(ctx context.Context, addrs []string) error {
	_, err := s.api.SetPeers(ctx, &cladefsdv1.SetPeersRequest{Addresses: addrs})
	return mapErr("set peers", err)
}

// ListCells lists cells currently open/served by cladefsd.
func (s *Store) ListCells(ctx context.Context) ([]*cladefsdv1.ListedCell, error) {
	resp, err := s.api.ListCells(ctx, &cladefsdv1.ListCellsRequest{})
	if err != nil {
		return nil, mapErr("list cells", err)
	}
	return resp.GetStores(), nil
}

// DecommissionHost retires ephemeral cell references left by a permanently lost
// host incarnation.
func (s *Store) DecommissionHost(ctx context.Context, incarnation uint64) (retired, left uint64, err error) {
	resp, err := s.api.DecommissionHost(ctx, &cladefsdv1.DecommissionHostRequest{Incarnation: incarnation})
	if err != nil {
		return 0, 0, mapErr("decommission host", err)
	}
	return resp.GetRetired(), resp.GetLeft(), nil
}

// Incarnation returns this cladefsd daemon's ephemeral host incarnation ID
// (0 if the daemon was started without CLADEFSD_EPHEMERAL).
func (s *Store) Incarnation(ctx context.Context) (uint64, error) {
	resp, err := s.api.GetStats(ctx, &cladefsdv1.GetStatsRequest{
		LeaveOutVms:        true,
		LeaveOutGuests:     true,
		LeaveOutHistograms: true,
	})
	if err != nil {
		return 0, mapErr("get stats", err)
	}
	return resp.GetIncarnation(), nil
}

func isNotFound(err error) bool {
	if errors.Is(err, syscall.ENOENT) {
		return true
	}
	if errno, ok := cladefsdv1.ErrnoOf(err); ok && errno == cladefsdv1.ENOENT {
		return true
	}
	return status.Code(err) == codes.NotFound
}

func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return fmt.Errorf("cladefs %s: %w: %v", op, storage.ErrNotFound, err)
	}
	if errors.Is(err, syscall.EEXIST) || status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("cladefs %s: %w: %v", op, storage.ErrExists, err)
	}
	if errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("cladefs %s: %w: %v", op, storage.ErrBusy, err)
	}
	if errno, ok := cladefsdv1.ErrnoOf(err); ok {
		switch errno {
		case cladefsdv1.EEXIST:
			return fmt.Errorf("cladefs %s: %w: %v", op, storage.ErrExists, err)
		case cladefsdv1.EBUSY:
			return fmt.Errorf("cladefs %s: %w: %v", op, storage.ErrBusy, err)
		}
	}
	return fmt.Errorf("cladefs %s: %w", op, err)
}
