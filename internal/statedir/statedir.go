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

	"github.com/semistrict/hotbed/internal/bench"
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
	// LocalUser is the unix user, besides root, who may call the controller
	// from its own machine; none when empty.
	LocalUser string `json:"localUser,omitempty"`
	// Updates has the controller follow CI's builds; none when unset.
	Updates *UpdatesConfig `json:"updates,omitempty"`
}

// UpdatesConfig is where CI publishes hotbed's builds, as GitHub releases.
type UpdatesConfig struct {
	// Repo is the GitHub repo, owner/name, such as semistrict/hotbed.
	Repo string `json:"repo"`
	// TokenFile holds a GitHub token that may read the repo's releases.
	TokenFile string `json:"tokenFile"`
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
	// Slice is the systemd slice commands run in; empty for hotbed.slice.
	Slice string `json:"slice,omitempty"`
	// Bench is the node's measurements (hotbed run --bench), as the host's
	// setup step sets them; none taken without.
	Bench *bench.Config `json:"bench,omitempty"`
	// Owner is the tailnet login the cluster belongs to: the worker admits
	// its machines.
	Owner string `json:"owner,omitempty"`
	// ControllerNodeID is the controller's stable tailnet node ID: only it may
	// call CreateWorkspace on this worker.
	ControllerNodeID string `json:"controllerNodeId,omitempty"`
	// Token is the bearer token used to authenticate to a remote HTTPS
	// controller (such as Cloudflare Workers) and its /ws/worker tunnel.
	Token string `json:"token,omitempty"`
}

func (c Config) validate() error {
	if (c.Controller == nil) == (c.Worker == nil) {
		return errors.New("config must set exactly one of controller and worker")
	}
	return nil
}

// Prepare makes dir if it is missing and checks that it holds no config. The
// state directory is its owner's alone (0700); the parents it makes are
// anyone's to pass through (0755, less the umask), as a worker's sibling
// directories under them are: a parent made 0700 too, as /var/lib/hotbed was
// for the controller's /var/lib/hotbed/controller, kept the worker's
// unprivileged commands and git out of /var/lib/hotbed/worker below it.
func Prepare(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create state directory's parent: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
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

// Rewrite replaces dir's config with cfg, which never leaves it partly
// written: a new file renamed over the old.
func Rewrite(dir string, cfg Config) error {
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
	if err := os.Rename(tmp.Name(), filepath.Join(dir, configName)); err != nil {
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
