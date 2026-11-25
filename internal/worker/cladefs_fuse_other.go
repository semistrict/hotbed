//go:build !linux

package worker

func clearMountNosuid(string) error { return nil }

func cleanupFUSEMountDir(string, bool) {}
func checkMountHealth(string) error    { return nil }
func isMountNotConnected(error) bool   { return false }
func detachDeadMount(string) error     { return nil }
