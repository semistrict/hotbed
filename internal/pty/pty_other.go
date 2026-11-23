//go:build !linux && !darwin

package pty

import (
	"errors"
	"os"
)

func openPTY() (*os.File, *os.File, string, error) {
	return nil, nil, "", errors.New("pty not supported on this platform")
}

func resizePTY(*os.File, uint16, uint16) error {
	return errors.New("pty not supported on this platform")
}
