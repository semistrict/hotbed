//go:build !darwin && !linux

package daemon

import (
	"context"
	"fmt"
	"runtime"
)

func InstallService(context.Context, ServiceConfig) (ServiceStatus, error) {
	return ServiceStatus{}, fmt.Errorf("background service install is not supported on %s; run `hotbed daemon` directly", runtime.GOOS)
}

func UninstallService(context.Context, string) (ServiceStatus, error) {
	return ServiceStatus{}, fmt.Errorf("background service uninstall is not supported on %s", runtime.GOOS)
}

func GetServiceStatus(_ context.Context, baseDir string) (ServiceStatus, error) {
	statusPath, _ := StatusFilePath(baseDir)
	return ServiceStatus{
		Platform:   runtime.GOOS,
		StatusFile: statusPath,
		Live:       LoadLiveSnapshot(statusPath),
	}, nil
}
