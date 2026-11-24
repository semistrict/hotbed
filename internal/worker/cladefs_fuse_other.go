//go:build !linux

package worker

func clearMountNosuid(string) error { return nil }
