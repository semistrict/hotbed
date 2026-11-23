//go:build !linux

package storage

import (
	"context"
	"fmt"
	"runtime"
)

// Open fails: workers, and so stores, are Linux only.
func Open(context.Context, string, string) (Store, error) {
	return nil, fmt.Errorf("storage needs Linux; this is %s", runtime.GOOS)
}
