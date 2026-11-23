//go:build !linux

package unit

func defaultHasSELinux() bool { return false }
