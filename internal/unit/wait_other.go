//go:build !linux

package unit

import (
	"context"
	"errors"
)

// Wait needs Linux's cgroups; workers run only there.
func Wait(context.Context, string) error { return errors.New("units need Linux") }
