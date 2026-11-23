//go:build !linux

package bench

import "errors"

func mountResctrl(string) error { return errors.New("resctrl needs Linux") }
