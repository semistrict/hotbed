//go:build !linux

package guestd

import (
	"errors"
	"syscall"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
)

type cmdCgroup struct {
	path string
}

func hasCgroupV2() bool                                                { return false }
func createCmdCgroup(string, *guestdv1.StartCommandRequest) *cmdCgroup { return nil }
func (c *cmdCgroup) addPID(int) error                        { return nil }
func attachCgroupAttr(*syscall.SysProcAttr, *cmdCgroup) (bool, func()) {
	return false, func() {}
}
func (c *cmdCgroup) freeze() error                { return errors.New("cgroup v2 requires Linux") }
func (c *cmdCgroup) thaw() error                  { return errors.New("cgroup v2 requires Linux") }
func (c *cmdCgroup) killAll()                     {}
func (c *cmdCgroup) reclaim(int64) error          { return nil }
func dropPageCaches() error                       { return nil }
func compactMemory() error                        { return nil }
func (c *cmdCgroup) remove() {}
func (c *cmdCgroup) stats() cmdCgroupStats {
	if c == nil {
		return cmdCgroupStats{}
	}
	return readCgroupStats(c.path)
}
func setCmdOOMScoreAdj(int, int) {}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func syncFilesystems()      {}
func powerOffGuest() error  { return errors.New("poweroff requires Linux") }
func kernelRelease() string { return "" }
