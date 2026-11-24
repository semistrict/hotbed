//go:build !linux

package main

import "log/slog"

func runPID1Supervisor(*slog.Logger) bool { return false }
