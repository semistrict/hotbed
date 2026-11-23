package baseimage

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/cmdlog"
	"github.com/semistrict/hotbed/internal/unit"
)

// SystemdProvisioner returns a Provisioner that executes the base-image
// provisioning script as root inside rootDir via a transient systemd unit
// (RootDirectory=rootDir, MountAPIVFS=yes, /etc/resolv.conf bound), logging
// output under logDir. The unit is named for the image, so a later worker
// process can find one an earlier process left running.
func SystemdProvisioner(slice, logDir string) Provisioner {
	return systemdProvisioner{slice: slice, logDir: logDir}
}

type systemdProvisioner struct{ slice, logDir string }

func provisionUnit(key string) string { return "hotbed-" + key }

func (p systemdProvisioner) Abandon(ctx context.Context, key string) error {
	if err := unit.Stop(ctx, provisionUnit(key)); err != nil && !strings.Contains(err.Error(), "not loaded") {
		return err
	}
	return nil
}

func (p systemdProvisioner) Provision(ctx context.Context, key, rootDir, script string) error {
	name := provisionUnit(key)
	logPath := filepath.Join(p.logDir, name+".log")
	if err := unit.Start(ctx, unit.Spec{
		Name:    name,
		Slice:   p.slice,
		RootDir: rootDir,
		Dir:     "/",
		User:    "root",
		Argv:    []string{"/bin/bash", "-c", script},
		Log:     logPath,
	}); err != nil {
		return err
	}
	defer func() {
		if err := unit.Stop(context.WithoutCancel(ctx), name); err != nil && !strings.Contains(err.Error(), "not loaded") {
			slog.Warn("stop base-image provision unit", "unit", name, "err", err)
		}
		if err := unmountSubmounts(rootDir); err != nil {
			slog.Warn("unmount leftover base-image provision submounts", "unit", name, "root", rootDir, "err", err)
		}
	}()
	if err := unit.Wait(ctx, p.slice, name); err != nil {
		return err
	}
	st, err := unit.Status(ctx, name)
	if err != nil {
		return err
	}
	if !st.Exited || st.ExitCode != 0 || st.Signal != 0 {
		tail := readLogTail(logPath, 4096)
		if tail != "" {
			return fmt.Errorf("base-image provision unit %s failed (exit=%d signal=%d):\n%s", name, st.ExitCode, st.Signal, tail)
		}
		return fmt.Errorf("base-image provision unit %s failed (exit=%d signal=%d)", name, st.ExitCode, st.Signal)
	}
	return nil
}

// readLogTail is the end of a log: its last lines in at most maxBytes.
func readLogTail(path string, maxBytes int) string {
	lines, _, err := cmdlog.Tail(path, maxBytes, maxBytes)
	if err != nil {
		slog.Warn("read base-image provision log", "path", path, "err", err)
		return ""
	}
	return strings.Join(lines, "\n")
}
