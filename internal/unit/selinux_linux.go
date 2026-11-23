//go:build linux

package unit

import "os"

func defaultHasSELinux() bool {
	_, err := os.Stat("/sys/fs/selinux/enforce")
	return err == nil
}
