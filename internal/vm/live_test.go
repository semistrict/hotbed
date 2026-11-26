package vm_test

import (
	"context"

	"github.com/semistrict/hotbed/internal/guestd"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/vm"
)

// liveResolver resolves the live tests' workspaces to their Manager's VMs.
type liveResolver struct{ mgr *vm.Manager }

func (r *liveResolver) EnsureVM(_ context.Context, wsID string, _ unit.VMSize) (*guestd.Client, error) {
	return r.mgr.ClientFor(wsID)
}
func (r *liveResolver) ClientFor(wsID string) (*guestd.Client, error) {
	return r.mgr.ClientFor(wsID)
}
func (r *liveResolver) StopVM(ctx context.Context, wsID string) error {
	return r.mgr.StopVM(ctx, wsID)
}
