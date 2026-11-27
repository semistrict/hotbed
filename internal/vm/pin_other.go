//go:build !linux

package vm

import "errors"

// setThreadAffinity is Linux's: Firecracker runs nowhere else.
func setThreadAffinity(tid, cpu int) error {
	return errors.New("pinning a thread to a CPU needs Linux")
}
