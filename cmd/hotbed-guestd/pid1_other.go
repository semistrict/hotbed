//go:build !linux

package main

import "log/slog"

func initPID1(*slog.Logger) error { return nil }
