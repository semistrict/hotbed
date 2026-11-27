package worker

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/daemonlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

const (
	logPushBatchLimit   = 200
	logPushBatchBytes   = 256 << 10 // 256 KiB per RPC
	logPushUrgentCount  = 50        // flush immediately once 50 INFO entries buffer
	logPushDebounceWait = time.Second
	logPushRPCTimeout   = 15 * time.Second
)

// logPusher drains a worker's daemonlog.Store stream buffer into that worker's
// dedicated per-org WorkerDO via ClusterService.PushDaemonLogs.
type logPusher struct {
	logs        *daemonlog.Store
	cluster     hotbedv1connect.ClusterServiceClient
	clusterID   string
	unsupported bool
}

func newLogPusher(eng *Engine, cluster hotbedv1connect.ClusterServiceClient, clusterID string) *logPusher {
	var logs *daemonlog.Store
	if eng != nil {
		logs = eng.cfg.DaemonLogs
	}
	return &logPusher{logs: logs, cluster: cluster, clusterID: clusterID}
}

func (p *logPusher) once(ctx context.Context) (more bool, err error) {
	if p.logs == nil || p.unsupported {
		return false, nil
	}
	batch := p.logs.PendingStream(logPushBatchLimit, logPushBatchBytes)
	if len(batch) == 0 {
		return false, nil
	}
	rpcCtx, cancel := context.WithTimeout(ctx, logPushRPCTimeout)
	defer cancel()
	resp, err := p.cluster.PushDaemonLogs(rpcCtx, &pb.PushDaemonLogsRequest{
		ClusterId: p.clusterID,
		Epoch:     p.logs.Epoch(),
		Entries:   batch,
	})
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			if !p.unsupported {
				slog.Info("controller does not support PushDaemonLogs yet; skipping worker log streaming")
				p.unsupported = true
			}
			return false, nil
		}
		return false, err
	}
	p.unsupported = false
	ack := resp.GetAckedSeq()
	if last := batch[len(batch)-1].GetSeq(); ack < last {
		ack = last
	}
	p.logs.AckStream(ack)
	remaining, _ := p.logs.StreamStats()
	return remaining > 0, nil
}

// runLogPusher streams INFO/WARN/ERROR slog records to WorkerDO until ctx ends:
// WARN/ERROR and bursts >= logPushUrgentCount flush immediately; low-volume
// INFO trickles coalesce for logPushDebounceWait to keep DO RPC rate low.
func runLogPusher(ctx context.Context, p *logPusher, wake <-chan struct{}, onFail func(error)) error {
	if p.logs == nil {
		<-ctx.Done()
		return nil
	}
	kicked := p.logs.StreamKicked()
	failures := 0
	for {
		if wake != nil {
			select {
			case <-wake:
			default:
			}
		}
		more, err := p.once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e := api.FromConnect(err)
			switch {
			case e.Code == api.CodeNotJoined || e.Code == api.CodeClusterMismatch:
				return e
			case failures == 0 || failures%20 == 0:
				slog.Warn("push daemon logs failed; retrying", "err", e, "failures", failures+1)
			}
			if onFail != nil {
				onFail(err)
			}
			failures++
			wait := min(time.Second<<min(failures-1, 6), maxRetryWait)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		failures = 0
		if more {
			continue
		}
		count, urgent := p.logs.StreamStats()
		if count > 0 && (urgent || count >= logPushUrgentCount) {
			continue
		}
		wait := api.ReportInterval
		if count > 0 && !p.unsupported {
			wait = logPushDebounceWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-kicked:
			timer.Stop()
			// If only a trickle of INFO arrived, coalesce briefly before pushing.
			if c, u := p.logs.StreamStats(); c > 0 && !u && c < logPushUrgentCount && !p.unsupported {
				dt := time.NewTimer(logPushDebounceWait)
				select {
				case <-ctx.Done():
					dt.Stop()
					return nil
				case <-wake:
					dt.Stop()
				case <-dt.C:
				}
			}
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
