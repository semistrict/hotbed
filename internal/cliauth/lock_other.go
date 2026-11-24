//go:build !unix

package cliauth

func withFileLock(_ string, fn func() error) error {
	return fn()
}
