//go:build !linux

package main

import "log/slog"

func runPID1Supervisor(*slog.Logger, uint64) bool { return false }
