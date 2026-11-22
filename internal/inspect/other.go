//go:build !linux

package inspect

import (
	"context"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Linux is unavailable on non-Linux hosts.
type Linux struct{}

func (Linux) Capabilities() []*pb.ToolCapability { return nil }

func (Linux) Inspect(context.Context, Target, *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error) {
	return nil, api.Errorf(api.CodePlatformNotSupported, "process inspection requires a Linux worker")
}

func (Linux) Trace(context.Context, Target, *pb.TraceCommandRequest, Emit) error {
	return api.Errorf(api.CodePlatformNotSupported, "process tracing requires a Linux worker")
}
