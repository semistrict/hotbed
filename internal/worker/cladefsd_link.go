package worker

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/cladefs"
)

// cladefsdStartWait bounds how long an upgrade waits for a restarted
// cladefsd to answer. cladefsd recovers its cells before it serves its
// control socket: 32s on vm1 (2026-11-26). It only bounds what the upgrade
// reports: a daemon that answers later is still picked up, since every
// heartbeat follows its incarnation (cladefsdLink.sync).
const cladefsdStartWait = 2 * time.Minute

// cladefsdLink is a worker's view of the cladefsd it talks to: the daemon's
// incarnation, the peer mesh applied to it, and the retired hosts it has
// decommissioned. A daemon that restarts, in an upgrade or by systemd after
// it died, comes back with a new incarnation and none of that applied, so
// sync reads the incarnation on every heartbeat and starts afresh on a
// change. Its zero value is a link to a daemon of incarnation 0.
type cladefsdLink struct {
	mu             sync.Mutex
	incarnation    uint64
	peers          []string // as last applied (SetPeers), sorted
	decommissioned map[uint64]bool
}

func (l *cladefsdLink) Incarnation() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.incarnation
}

// Peers returns the peer mesh last applied to this incarnation.
func (l *cladefsdLink) Peers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.peers)
}

// restarted records the daemon's incarnation, forgetting what was applied
// to an earlier one.
func (l *cladefsdLink) restarted(incarnation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if incarnation == l.incarnation {
		return
	}
	slog.Info("cladefsd restarted: applying the peer mesh to it again", "incarnation", incarnation, "was", l.incarnation)
	l.incarnation = incarnation
	l.peers = nil
	l.decommissioned = nil
}

// sync follows the daemon's incarnation, applies the controller's peer mesh
// (when this host serves peers) if this incarnation lacks it, and
// decommissions the retired hosts not yet done.
func (l *cladefsdLink) sync(ctx context.Context, st *cladefs.Store, peerAddr string, peers []string, decommission []uint64) {
	if inc, err := st.Incarnation(ctx); err != nil {
		slog.Warn("read cladefsd incarnation", "err", err)
	} else {
		l.restarted(inc)
	}
	if len(peers) > 0 && peerAddr != "" {
		sorted := slices.Sorted(slices.Values(peers))
		if !slices.Equal(l.Peers(), sorted) {
			if err := st.SetPeers(ctx, sorted); err != nil {
				slog.Warn("cladefsd SetPeers failed", "peers", sorted, "err", err)
			} else {
				l.mu.Lock()
				l.peers = sorted
				l.mu.Unlock()
				slog.Info("updated cladefsd peer mesh", "peers", sorted)
			}
		}
	}
	for _, inc := range decommission {
		l.mu.Lock()
		skip := inc == 0 || inc == l.incarnation || l.decommissioned[inc]
		l.mu.Unlock()
		if skip {
			continue
		}
		retired, left, err := st.DecommissionHost(ctx, inc)
		if err != nil {
			slog.Warn("cladefsd DecommissionHost failed", "incarnation", inc, "err", err)
			continue
		}
		slog.Info("decommissioned retired cladefs host", "incarnation", inc, "retired", retired, "left", left)
		if left == 0 {
			l.mu.Lock()
			if l.decommissioned == nil {
				l.decommissioned = map[uint64]bool{}
			}
			l.decommissioned[inc] = true
			l.mu.Unlock()
		}
	}
}
