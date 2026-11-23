//go:build !linux

package baseimage

func unmountSubmounts(string) error { return nil }
