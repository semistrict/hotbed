//go:build !linux && !darwin

package unit

import "errors"

// waitExited cannot wait for an exit without reaping here: ProcGroup.Wait
// reaps first.
func waitExited(int) error { return errors.New("waiting without reaping needs Linux or macOS") }
