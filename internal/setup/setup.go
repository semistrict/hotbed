// Package setup provisions a hotbed worker node on the current machine in a
// single command: directly on Linux (any filesystem; workspaces run in VMs on
// cladefs), and on macOS natively too, every command in a VM of machined's,
// which hotbed's daemon runs beside the worker (internal/macworker).
package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/macworker"
)

const (
	// DaemonLaunchAgentLabel is the unified macOS LaunchAgent label for `hotbed daemon`.
	DaemonLaunchAgentLabel = "com.semistrict.hotbed.daemon"
)

// Options configure single-command worker setup on Linux or macOS.
type Options struct {
	Controller     string
	Token          string // optional bearer/join token for remote HTTPS controllers
	Dir            string // base directory; default ~/.hotbed
	User           string // unix user commands run as; default current/SUDO_USER
	UnitProperties []string
	Slice          string
	NoStart        bool
	// Hostname is the node name Setup reports for a worker already joined
	// whose state names none (Linux).
	Hostname string

	// Progress receives human-readable progress lines (typically stderr).
	Progress io.Writer
}

// Result is returned by Setup as the CLI envelope data.
type Result struct {
	Platform   string `json:"platform"` // "linux" or "darwin"
	Dir        string `json:"dir"`
	StateDir   string `json:"stateDir"`
	Controller string `json:"controller"`
	User       string `json:"user"`
	Node       string `json:"node,omitempty"`
	ClusterID  string `json:"clusterId,omitempty"`
	Storage    string `json:"storage"` // "cladefs"
	Running    bool   `json:"running"`
	Service    string `json:"service,omitempty"`
	// Mac is how a Mac's worker and machined stand (hotbed's daemon runs
	// them).
	Mac  *macworker.State `json:"mac,omitempty"`
	Next []string         `json:"next,omitempty"`
}

// Status describes a locally configured worker.
type Status struct {
	Platform   string `json:"platform"`
	Dir        string `json:"dir"`
	StateDir   string `json:"stateDir"`
	Controller string `json:"controller,omitempty"`
	User       string `json:"user,omitempty"`
	Storage    string `json:"storage,omitempty"`
	Running    bool   `json:"running"`
	Service    string `json:"service,omitempty"`
	// Mac is how a Mac's worker and machined stand (hotbed's daemon runs
	// them).
	Mac *macworker.State `json:"mac,omitempty"`
}

// LocalWorkerSocket is a worker on this machine that serves its clients
// here on a Unix socket: those of its node's workspaces and commands call it
// there, the rest through the controller.
type LocalWorkerSocket struct {
	NodeID string
	Socket string
}

// DestroyOptions control worker teardown.
type DestroyOptions struct {
	Dir      string
	Progress io.Writer
}

// DestroyResult summarizes what Destroy removed.
type DestroyResult struct {
	Dir     string   `json:"dir"`
	Removed []string `json:"removed"`
}

// DefaultDir returns ~/.hotbed for the invoking user (respecting SUDO_USER on
// Linux so `sudo hotbed worker init` targets the user's home, not /root).
func DefaultDir() (string, error) {
	home, _, err := invokingUserHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hotbed"), nil
}

// DefaultUser returns the invoking non-root username when available.
func DefaultUser() (string, error) {
	_, name, err := invokingUserHome()
	return name, err
}

func invokingUserHome() (home, username string, err error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			return u.HomeDir, u.Username, nil
		}
	}
	u, err := user.Current()
	if err != nil {
		h, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", fmt.Errorf("determine home directory: %w", err)
		}
		return h, os.Getenv("USER"), nil
	}
	return u.HomeDir, u.Username, nil
}

// ResolveDir expands an empty path to DefaultDir and a leading ~/ to the
// invoking user's home directory, returning an absolute path.
func ResolveDir(dir string) (string, error) {
	if dir == "" {
		return DefaultDir()
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, _, err := invokingUserHome()
		if err != nil {
			return "", err
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[2:])
		}
	}
	return filepath.Abs(dir)
}

// JoinedController is the controller `hotbed worker init` joined this
// machine's worker to, as it recorded it in dir (default ~/.hotbed): what
// client commands call. It is "" when no worker was set up there.
func JoinedController(dir string) (string, error) {
	baseDir, err := ResolveDir(dir)
	if err != nil {
		return "", err
	}
	return joinedController(baseDir)
}

// controllerRecord is the controller a worker init's record at path names:
// "" if there is none.
func controllerRecord(path string) (string, error) {
	var rec struct {
		Controller string `json:"controller"`
	}
	if err := readJSON(path, &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return rec.Controller, nil
}

func progressf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "hotbed: "+format+"\n", args...)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func copyFileAtomic(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("source %s is not a regular file", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if os.Geteuid() == 0 {
		if err := tmp.Chown(0, 0); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
