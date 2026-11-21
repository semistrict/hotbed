//go:build !linux

package worker

import (
	"runtime"

	"github.com/semistrict/hotbed/internal/api"
)

// Probe fails: workers run work in cgroups and btrfs snapshots, which need Linux.
func Probe() (api.Resources, error) {
	return api.Resources{}, api.Errorf(api.CodePlatformNotSupported, "workers need Linux; this is %s", runtime.GOOS).
		WithSuggestion("run the controller or the client here, and workers on Linux machines")
}
