package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/setup"
)

const (
	LaunchAgentLabel       = setup.DaemonLaunchAgentLabel
	LegacyT3BridgeLabel    = "com.semistrict.hotbed.t3-bridge"
	SystemdUnitName        = "hotbed-daemon.service"
	LegacyT3BridgeUnitName = "hotbed-t3-bridge.service"
)

// T3BridgeSummary is the T3 Code question notification bridge status embedded
// inside Snapshot.
type T3BridgeSummary struct {
	Enabled          bool      `json:"enabled"`
	Available        bool      `json:"available"`
	State            string    `json:"state"` // "connected", "reconnecting", "unavailable", "disabled"
	Binary           string    `json:"binary,omitempty"`
	PendingQuestions int       `json:"pendingQuestions"`
	TrackedQuestions int       `json:"trackedQuestions"`
	LastWakeAt       time.Time `json:"lastWakeAt,omitempty"`
	LastWakeThread   string    `json:"lastWakeThread,omitempty"`
	LastWakeIDs      []int32   `json:"lastWakeIds,omitempty"`
	LastError        string    `json:"lastError,omitempty"`
}

// Snapshot is the live status published by `hotbed daemon` to both
// ~/.hotbed/daemon-status.json and the macOS menubar app.
type Snapshot struct {
	Version    string              `json:"version"`
	Build      int64               `json:"build"`
	PID        int                 `json:"pid"`
	UpdatedAt  time.Time           `json:"updatedAt"`
	Controller string              `json:"controller,omitempty"`
	CockpitURL string              `json:"cockpitUrl,omitempty"`
	LogPath    string              `json:"logPath,omitempty"`
	T3Bridge   T3BridgeSummary     `json:"t3Bridge"`
	WorkerVM   setup.DaemonVMState `json:"workerVm"`
}

// ServiceConfig configures installing the background `hotbed daemon` service.
type ServiceConfig struct {
	BaseDir string
	Binary  string
	Args    []string
	LogPath string
}

// ServiceStatus describes the local background `hotbed daemon` service and its
// latest live status snapshot (if running).
type ServiceStatus struct {
	Platform   string    `json:"platform"`
	Installed  bool      `json:"installed"`
	Running    bool      `json:"running"`
	Label      string    `json:"label,omitempty"`
	UnitPath   string    `json:"unitPath,omitempty"`
	LogPath    string    `json:"logPath,omitempty"`
	StatusFile string    `json:"statusFile,omitempty"`
	Live       *Snapshot `json:"live,omitempty"`
}

// DefaultLogPath returns <baseDir>/daemon.log (default ~/.hotbed/daemon.log).
func DefaultLogPath(baseDir string) (string, error) {
	dir, err := setup.ResolveDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

// StatusFilePath returns <baseDir>/daemon-status.json.
func StatusFilePath(baseDir string) (string, error) {
	dir, err := setup.ResolveDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon-status.json"), nil
}

// SaveSnapshot atomically writes snap to path.
func SaveSnapshot(path string, snap Snapshot) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, "daemon-status.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadLiveSnapshot reads path and returns the snapshot if its PID is still alive.
func LoadLiveSnapshot(path string) *Snapshot {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil
	}
	if snap.PID <= 0 || !pidAlive(snap.PID) {
		return nil
	}
	return &snap
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
