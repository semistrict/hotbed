//go:build !linux

package unit

import "errors"

func prepareChrootTargets(string, []Bind) error {
	return errors.New("chroot bind mountpoints need Linux")
}
