package t3bridge

import (
	"path/filepath"

	"github.com/semistrict/hotbed/internal/setup"
)

const (
	LaunchAgentLabel = "com.semistrict.hotbed.t3-bridge"
	SystemdUnitName  = "hotbed-t3-bridge.service"
)

// ServiceConfig configures installing the background t3-bridge service.
type ServiceConfig struct {
	Binary  string
	Args    []string
	LogPath string
}

// ServiceStatus describes the local background t3-bridge service state.
type ServiceStatus struct {
	Platform   string `json:"platform"`
	Installed  bool   `json:"installed"`
	Running    bool   `json:"running"`
	Label      string `json:"label,omitempty"`
	UnitPath   string `json:"unitPath,omitempty"`
	LogPath    string `json:"logPath,omitempty"`
	StateFile  string `json:"stateFile,omitempty"`
	TrackedIDs int    `json:"trackedQuestions"`
	Controller string `json:"controller,omitempty"`
	T3Binary   string `json:"t3Binary,omitempty"`
}

// DefaultLogPath returns ~/.hotbed/t3-bridge.log.
func DefaultLogPath() (string, error) {
	dir, err := setup.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "t3-bridge.log"), nil
}
