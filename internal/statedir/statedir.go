// Package statedir reads and writes a node's state directory. The directory's
// config file is the worker's; writing it is the last step of `hotbed join`,
// so a directory with a config is complete.
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

// Path is where the state directory dir keeps its config.
func Path(dir string) string { return filepath.Join(dir, configName) }

var (
	ErrAlreadyInitialized = errors.New("state directory is already initialized")
	ErrNotInitialized     = errors.New("state directory is not initialized")
)

// Config is a node's role: a worker's.
type Config struct {
	Worker *WorkerConfig `json:"worker,omitempty"`
}

// WorkerConfig is the cluster a worker joined and how it runs work.
type WorkerConfig struct {
	ClusterID  string `json:"clusterId"`
	Controller string `json:"controller"` // host:port
	// User runs every command; the worker itself runs as root.
	User string `json:"user"`
	// UnitProperties are systemd properties every command gets.
	UnitProperties []string `json:"unitProperties,omitempty"`
	// Slice is the systemd slice commands run in; empty for hotbed.slice.
	Slice string `json:"slice,omitempty"`
	// Bench is the node's measurements (hotbed run --bench), as the host's
	// setup step sets them; none taken without.
	Bench *bench.Config `json:"bench,omitempty"`
	// Owner is the login the cluster belongs to: who the worker names the
	// controller's calls as coming from.
	Owner string `json:"owner,omitempty"`
	// ControllerNodeID is the node ID the worker names the controller's
	// calls, over its tunnel, as coming from: only it may call
	// CreateWorkspace on this worker.
	ControllerNodeID string `json:"controllerNodeId,omitempty"`
	// Token is the bearer token used to authenticate to a remote HTTPS
	// controller (such as Cloudflare Workers) and its /ws/worker tunnel.
	Token string `json:"token,omitempty"`
	// Socket, if set, is a Unix socket the worker serves its API on too, for
	// clients on its own machine: its file mode alone says who may connect
	// (macOS: the Mac's user, 0600), and its callers are this node itself.
	Socket string `json:"socket,omitempty"`
	// NodeID is the worker's node, which its worker token is bound to (its
	// workspaces' and commands' node_id): made at join.
	NodeID string `json:"nodeId,omitempty"`
	// Host is its node's name, as the controller and people know it.
	Host string `json:"host,omitempty"`
	// VM enables the cladefsd + Firecracker microVM runtime alongside chroot.
	VM *VMConfig `json:"vm,omitempty"`
	// CladefsFUSE enables the cladefsd host-FUSE runtime alongside chroot
	// (used on arm64 / non-KVM hosts such as 16 KiB Apple Silicon or QEMU VMs).
	CladefsFUSE *CladefsFUSEConfig `json:"cladefsFuse,omitempty"`
	// Freshen configures base freshen duty-cycle caps, cooldowns, and time limits.
	Freshen *FreshenConfig `json:"freshen,omitempty"`
}

// FreshenConfig configures worker base freshen throttling and safety limits.
// Pointer fields distinguish explicitly set zeros from unset defaults.
type FreshenConfig struct {
	MaxDutyCycle               *float64 `json:"maxDutyCycle,omitempty"`
	MinIntervalSeconds         *int     `json:"minIntervalSeconds,omitempty"`
	MaxCooldownSeconds         *int     `json:"maxCooldownSeconds,omitempty"`
	FailureCooldownSeconds     *int     `json:"failureCooldownSeconds,omitempty"`
	MaxOptionalDurationSeconds *int     `json:"maxOptionalDurationSeconds,omitempty"`
	MaxRequiredDurationSeconds *int     `json:"maxRequiredDurationSeconds,omitempty"`
	IdleDelaySeconds           *int     `json:"idleDelaySeconds,omitempty"`
	DisableOptional            *bool    `json:"disableOptional,omitempty"`
}

// CladefsFUSEConfig configures a worker's cladefsd host-FUSE runtime
// (R2-backed cells mounted via CLADEFSD_MOUNT, local SSD strictly as cache).
type CladefsFUSEConfig struct {
	Enabled            bool   `json:"enabled,omitempty"`
	Default            bool   `json:"default,omitempty"` // default non-bench workspaces to "cladefs"
	CladefsControlSock string `json:"cladefsControlSock,omitempty"`
	CladefsPeerAddr    string `json:"cladefsPeerAddr,omitempty"`
	CellRoot           string `json:"cellRoot,omitempty"`
	MountRoot          string `json:"mountRoot,omitempty"`
	MountStoresRoot    string `json:"mountStoresRoot,omitempty"`
	TenantID           uint64 `json:"tenantId,omitempty"`
	DiskBytes          uint64 `json:"diskBytes,omitempty"`
}

// VMConfig configures a worker's cladefsd + Firecracker microVM runtime.
type VMConfig struct {
	Enabled            bool     `json:"enabled,omitempty"`
	Default            bool     `json:"default,omitempty"` // default non-bench workspaces to "vm"
	CladefsControlSock string   `json:"cladefsControlSock,omitempty"`
	CladefsVMSock      string   `json:"cladefsVMSock,omitempty"`
	CladefsPeerAddr    string   `json:"cladefsPeerAddr,omitempty"`
	CellRoot           string   `json:"cellRoot,omitempty"`
	TenantID           uint64   `json:"tenantId,omitempty"`
	FirecrackerBin     string   `json:"firecrackerBin,omitempty"`
	KernelPath         string   `json:"kernelPath,omitempty"`
	GuestdBin          string   `json:"guestdBin,omitempty"`
	VMUser             string   `json:"vmUser,omitempty"`
	VCPUs              int      `json:"vcpus,omitempty"`
	MemMiB             int      `json:"memMiB,omitempty"`
	DiskBytes          uint64   `json:"diskBytes,omitempty"`
	DAXWindowBytes     uint64   `json:"daxWindowBytes,omitempty"`
	DAXPrivateMaxBytes uint64   `json:"daxPrivateMaxBytes,omitempty"`
	DisableDAX         bool     `json:"disableDax,omitempty"` // omit virtio-extent DAX window (e.g. 16 KiB arm64 hosts)
	CacheType          string   `json:"cacheType,omitempty"`  // virtio-extent cache_type: "Unsafe" (default, non-blocking fsync) or "Writeback"
	BridgeName         string   `json:"bridgeName,omitempty"`
	BridgeCIDR         string   `json:"bridgeCIDR,omitempty"`
	DNSServers         []string `json:"dnsServers,omitempty"`
	Nofile             uint64   `json:"nofile,omitempty"`
	// MachinedSock is machined's API socket (macOS workers): machined's
	// machines then run the VMs, and the cladefs daemon it embeds, whose
	// Cladefsd service it serves on the same socket, holds their cells, in
	// place of Firecracker and cladefsd. The cladefsd, Firecracker, TAP and
	// tenant settings above are then unused.
	MachinedSock string `json:"machinedSock,omitempty"`
	// MachinedImages is machined's images directory (its --images), which
	// KernelPath must be in; the initramfses guestd boots from are made in
	// it.
	MachinedImages string `json:"machinedImages,omitempty"`
}

func (c Config) validate() error {
	if c.Worker == nil {
		return errors.New("config must set worker")
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
