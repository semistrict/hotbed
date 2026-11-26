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

// joinedController is "": worker init sets up no worker here.
func joinedController(string) (string, error) { return "", nil }

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

// LocalWorker is none: clients reach this machine's worker through the
// controller.
func LocalWorker(string) (LocalWorkerSocket, error) { return LocalWorkerSocket{}, nil }
