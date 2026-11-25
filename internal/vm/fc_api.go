package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrSuspendUnsupported indicates that the running Firecracker VM, guestd
// binary, or cladefsd daemon does not support memory-cell VM snapshotting
// (e.g., a pre-upgrade VM launched with --no-api, an older hotbed-guestd
// without FreezeFS, or a cladefsd build without ephemeral-cell snapshots).
var ErrSuspendUnsupported = errors.New("vm memory-cell suspend unsupported")

// MemoryCellSnapshot identifies the memory cell created or checkpointed by
// Firecracker's PUT /snapshot/create when snapshotting directly into a
// cladefsd memory cell (TASK-456.4). For host-local ephemeral memory cells,
// Ephemeral is true and Version is 0 (zero bucket uploads).
type MemoryCellSnapshot struct {
	Tenant    uint64 `json:"tenant"`
	Cell      uint64 `json:"cell"`
	Version   uint64 `json:"version"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

// CreateMemoryCellConfig configures PUT /snapshot/create's `memory_cell` field.
// For a cold-booted VM, both PathOnHost and CladefsdSocket must be set (with
// Ephemeral: true for host-local zero-bucket-upload parking). For a VM already
// restored from a memory cell (MemBackendType::Cell), both path fields must be
// omitted (marshaling to `{}`) so Firecracker writes only dirty private pages
// into its existing memory cell.
type CreateMemoryCellConfig struct {
	PathOnHost     string `json:"path_on_host,omitempty"`
	CladefsdSocket string `json:"cladefsd_socket,omitempty"`
	Ephemeral      bool   `json:"ephemeral,omitempty"`
}

// CreateSnapshotRequest matches Firecracker's CreateSnapshotParams schema.
type CreateSnapshotRequest struct {
	SnapshotType      string                  `json:"snapshot_type"`
	SnapshotPath      string                  `json:"snapshot_path"`
	SyncSnapshotFiles *bool                   `json:"sync_snapshot_files,omitempty"`
	MemoryCell        *CreateMemoryCellConfig `json:"memory_cell,omitempty"`
}

// MemBackendConfig matches Firecracker's MemBackendConfig schema.
type MemBackendConfig struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// MemoryCellConfig matches Firecracker's MemoryCellConfig schema for
// PUT /snapshot/load with backend_type "Cell". When Ephemeral is true,
// cladefsd reopens the existing host-local ephemeral memory cell in place with
// zero bucket I/O and Version must be 0.
type MemoryCellConfig struct {
	Tenant         uint64 `json:"tenant,omitempty"`
	Cell           uint64 `json:"cell,omitempty"`
	Version        uint64 `json:"version,omitempty"`
	CladefsdSocket string `json:"cladefsd_socket"`
	Ephemeral      bool   `json:"ephemeral,omitempty"`
}

// CladefsOverride overrides an extent-store device path on PUT /snapshot/load.
type CladefsOverride struct {
	ID             string `json:"id"`
	PathOnHost     string `json:"path_on_host"`
	CladefsdSocket string `json:"cladefsd_socket,omitempty"`
}

// NetworkOverride remaps a virtio-net host TAP device on PUT /snapshot/load.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// VsockOverride overrides the host vsock UDS path on PUT /snapshot/load.
type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// LoadSnapshotRequest matches Firecracker's LoadSnapshotConfig schema.
type LoadSnapshotRequest struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       MemBackendConfig  `json:"mem_backend"`
	MemoryCell       *MemoryCellConfig `json:"memory_cell,omitempty"`
	ResumeVM         bool              `json:"resume_vm"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *VsockOverride    `json:"vsock_override,omitempty"`
	CladefsOverrides []CladefsOverride `json:"cladefs_overrides,omitempty"`
}

type fcAPIClient struct {
	sockPath string
	hc       *http.Client
}

func newFCAPIClient(sockPath string) *fcAPIClient {
	return &fcAPIClient{
		sockPath: sockPath,
		hc: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
	}
}

type fcErrorBody struct {
	FaultMessage string `json:"fault_message"`
}

func (c *fcAPIClient) do(ctx context.Context, method, path string, reqBody any, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("firecracker %s %s: marshal request: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bodyReader)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(rawResp))
		var fe fcErrorBody
		if json.Unmarshal(rawResp, &fe) == nil && fe.FaultMessage != "" {
			msg = fe.FaultMessage
		}
		err := fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
		if isUnsupportedSnapshotFault(msg) {
			return fmt.Errorf("%w: %v", ErrSuspendUnsupported, err)
		}
		return err
	}
	if respOut != nil && len(bytes.TrimSpace(rawResp)) > 0 {
		if err := json.Unmarshal(rawResp, respOut); err != nil {
			return fmt.Errorf("firecracker %s %s: decode response (%s): %w", method, path, strings.TrimSpace(string(rawResp)), err)
		}
	}
	return nil
}

func isUnsupportedSnapshotFault(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "ephemeral") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "enotsup") ||
		strings.Contains(low, "unknown field `memory_cell`") ||
		strings.Contains(low, "unknown field `ephemeral`") ||
		strings.Contains(low, "unknown variant `cell`")
}

func (c *fcAPIClient) PatchVM(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", struct {
		State string `json:"state"`
	}{State: state}, nil)
}

func (c *fcAPIClient) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (MemoryCellSnapshot, error) {
	var out MemoryCellSnapshot
	if err := c.do(ctx, http.MethodPut, "/snapshot/create", req, &out); err != nil {
		return MemoryCellSnapshot{}, err
	}
	if out.Cell == 0 || (!out.Ephemeral && out.Version == 0) {
		return MemoryCellSnapshot{}, fmt.Errorf("firecracker PUT /snapshot/create returned incomplete memory cell snapshot: %+v", out)
	}
	return out, nil
}

func (c *fcAPIClient) LoadSnapshot(ctx context.Context, req LoadSnapshotRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", req, nil)
}

func waitAPISock(ctx context.Context, sockPath string, timeout time.Duration, checkExited func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var d net.Dialer
		d.Timeout = 250 * time.Millisecond
		conn, err := d.DialContext(ctx, "unix", sockPath)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if checkExited != nil {
			if exitErr := checkExited(); exitErr != nil {
				return exitErr
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for firecracker API socket %s after %s (last err: %v)", sockPath, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
