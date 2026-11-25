//go:build !linux

package worker

func clearMountNosuid(string) error { return nil }

func cleanupFUSEMountDir(string, bool) {}
