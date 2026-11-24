package worker

import (
	"fmt"
	"os"
	"path/filepath"
)

// containersConfChrootPath is where the worker bind-mounts its default
// containers.conf drop-in inside every command and shell chroot.
const containersConfChrootPath = "/etc/containers/containers.conf.d/50-hotbed.conf"

// containersConfContent configures rootless podman and buildah for chrooted
// hotbed commands, which run inside a transient systemd unit without a systemd
// user session (board 10735). Using cgroupfs keeps containers inside the
// command's own cgroup (so limits, freezing, and KillMode=control-group apply)
// instead of calling sd-bus for a missing user manager, and file event logging
// avoids an unavailable journald socket. User configs (~/.config/containers)
// and explicit CLI flags still take precedence.
const containersConfContent = `# Managed by hotbed. Commands run in a chrooted systemd unit without a
# systemd user session, so default rootless podman/buildah to cgroupfs
# (keeping containers inside the command's cgroup) and file event logs.
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
`

func writeContainersConf(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "containers.conf")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(containersConfContent), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("install %s: %w", path, err)
	}
	return path, nil
}
