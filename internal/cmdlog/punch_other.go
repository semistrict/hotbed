//go:build !linux && !darwin

package cmdlog

import (
	"errors"
	"os"
)

func punchHole(*os.File, int64, int64) error { return errors.ErrUnsupported }
