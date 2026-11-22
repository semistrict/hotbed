//go:build !linux && !darwin

package setup

import (
	"context"
	"net/http"
	"runtime"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/worker"
)

func unsupported() error {
	return api.Errorf(api.CodePlatformNotSupported, "worker setup is supported on Linux and macOS; this is %s", runtime.GOOS)
}

// workerRecord is "": worker init sets up no worker here.
func workerRecord(string) string { return "" }

func Setup(context.Context, Options, worker.Tailnet, *http.Client, worker.Check) (Result, error) {
	return Result{}, unsupported()
}

func GetStatus(context.Context, string) (Status, error) {
	return Status{}, unsupported()
}

func Start(context.Context, string) (Status, error) {
	return Status{}, unsupported()
}

func Stop(context.Context, string) (Status, error) {
	return Status{}, unsupported()
}

func Trim(context.Context, string) (TrimResult, error) {
	return TrimResult{}, unsupported()
}

func Destroy(context.Context, DestroyOptions) (DestroyResult, error) {
	return DestroyResult{}, unsupported()
}

func SSH(context.Context, string, []string) (int, error) {
	return 1, unsupported()
}
