//go:build !linux

package worker

import "context"

// EnsureUserfaultfdAccess is a Linux-only host configuration step.
func EnsureUserfaultfdAccess(context.Context, string) error { return nil }

// EnsureKVMAccess is a Linux-only host configuration step.
func EnsureKVMAccess(context.Context, string) error { return nil }
