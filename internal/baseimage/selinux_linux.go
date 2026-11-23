//go:build linux

package baseimage

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// selinuxRootfsLabel is applied to a newly created base-image root directory
// before OCI extraction on SELinux-enforcing hosts (e.g. Fedora). Fresh btrfs
// subvolumes carry no security.selinux xattr, so files extracted into them
// default to unlabeled_t, which systemd's init_t is denied from executing
// inside RootDirectory=. Labeling the root directory usr_t up front causes all
// extracted and subsequently created files to inherit usr_t automatically, and
// snapshots preserve it.
const selinuxRootfsLabel = "system_u:object_r:usr_t:s0"

func labelRootfs(rootDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		return nil
	}
	if err := unix.Lsetxattr(rootDir, "security.selinux", []byte(selinuxRootfsLabel+"\x00"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	return nil
}
