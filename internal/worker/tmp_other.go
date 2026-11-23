//go:build !linux

package worker

import "errors"

func detach(string) error { return errors.New("unmounting needs Linux") }
