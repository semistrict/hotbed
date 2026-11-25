//go:build !darwin && !linux

package t3bridge

import (
	"context"
	"fmt"
	"runtime"
)

func InstallService(context.Context, ServiceConfig) (ServiceStatus, error) {
	return ServiceStatus{}, fmt.Errorf("background service install is not supported on %s; run `hotbed t3-bridge` directly", runtime.GOOS)
}

func UninstallService(context.Context) (ServiceStatus, error) {
	return ServiceStatus{}, fmt.Errorf("background service uninstall is not supported on %s", runtime.GOOS)
}

func GetServiceStatus(context.Context) (ServiceStatus, error) {
	return ServiceStatus{Platform: runtime.GOOS}, nil
}
