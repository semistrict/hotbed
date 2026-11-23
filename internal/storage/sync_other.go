//go:build !linux

package storage

func flush(string) error { return nil }
