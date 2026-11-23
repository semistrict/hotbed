package worker_test

import (
	"os"
	"testing"

	"github.com/semistrict/hotbed/internal/worker"
)

// TestMain lets the test binary stand in for hotbed where the worker runs
// itself in a rootfs: hotbed __sync-apply.
func TestMain(m *testing.M) {
	worker.Main(os.Args[1:])
	os.Exit(m.Run())
}
