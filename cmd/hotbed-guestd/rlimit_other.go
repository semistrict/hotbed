//go:build !linux

package main

import "log/slog"

func applyNofileLimit(_ *slog.Logger, limit uint64) (uint64, error) {
	return limit, nil
}
