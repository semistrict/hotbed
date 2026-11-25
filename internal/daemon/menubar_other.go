//go:build !darwin

package daemon

import "context"

func (d *Daemon) runMenubar(ctx context.Context) {
	<-ctx.Done()
}
