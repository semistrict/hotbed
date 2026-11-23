//go:build !linux

package baseimage

func labelRootfs(string) error { return nil }
