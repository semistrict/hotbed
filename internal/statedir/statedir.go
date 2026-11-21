// Package statedir reads and writes a node's state directory. The directory's
// config file says which role the node has; writing it is the last step of
// `hotbed init` or `hotbed join`, so a directory with a config is complete.
package statedir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const configName = "hotbed.json"

var (
	ErrAlreadyInitialized = errors.New("state directory is already initialized")
	ErrNotInitialized     = errors.New("state directory is not initialized")
)

// Config is a node's role. Exactly one of Controller and Worker is set.
type Config struct {
	Controller *ControllerConfig `json:"controller,omitempty"`
	Worker     *WorkerConfig     `json:"worker,omitempty"`
}

// ControllerConfig is a cluster as its controller created it.
type ControllerConfig struct {
	ClusterID string `json:"clusterId"`
	// Owner is the tailnet login allowed to use the cluster: the owner of the
	// controller's own node when it was initialized.
	Owner string `json:"owner"`
	Port  int    `json:"port"`
}

// WorkerConfig is the cluster a worker joined and how it runs work.
type WorkerConfig struct {
	ClusterID  string `json:"clusterId"`
	Controller string `json:"controller"` // host:port
	// User runs every command; the worker itself runs as root.
	User string `json:"user"`
	// Dataset is the zfs dataset for volumes, when the state directory is on
	// zfs and the default (<pool>/hotbed) will not do.
	Dataset string `json:"dataset,omitempty"`
	// UnitProperties are systemd properties every command gets.
	UnitProperties []string `json:"unitProperties,omitempty"`
}

func (c Config) validate() error {
	if (c.Controller == nil) == (c.Worker == nil) {
		return errors.New("config must set exactly one of controller and worker")
	}
	return nil
}

// Prepare makes dir if it is missing and checks that it holds no config.
func Prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	_, err := os.Stat(filepath.Join(dir, configName))
	switch {
	case err == nil:
		return fmt.Errorf("%s: %w", dir, ErrAlreadyInitialized)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check state directory: %w", err)
	}
}

// Commit writes cfg as dir's config. It fails with ErrAlreadyInitialized if
// dir has one, and never leaves a partly written config behind: it writes a
// temporary file and hard-links it into place, which fails if the name exists.
func Commit(dir string, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, configName+".*")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Link(tmp.Name(), filepath.Join(dir, configName)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", dir, ErrAlreadyInitialized)
		}
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Load reads dir's config.
func Load(dir string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, configName))
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: %w", dir, ErrNotInitialized)
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, configName), err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", filepath.Join(dir, configName), err)
	}
	return cfg, nil
}
