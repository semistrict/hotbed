package api

import (
	"context"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

const (
	// StreamKeepalive is how often a server stream that may have nothing to
	// say for long (keepaliveStreams) sends something anyway: a quiet stream
	// is then told apart from a dead one.
	StreamKeepalive = 5 * time.Second
	// StreamSilence is how long HearKeepalives lets such a stream say
	// nothing before taking it for dead. It is well under the worker's
	// orphanAfter (30s), so a client following an attached command is back
	// on a new stream before the command is stopped as orphaned.
	StreamSilence = 4 * StreamKeepalive
)

// keepaliveStreams are the server streams that send a message at least
// every StreamKeepalive while they are open: FollowLog an empty LogChunk
// while it follows, WatchWorkspace the workspace again.
var keepaliveStreams = map[string]bool{
	hotbedv1connect.WorkspaceServiceFollowLogProcedure:      true,
	hotbedv1connect.WorkspaceServiceWatchWorkspaceProcedure: true,
}

// HearKeepalives is a client interceptor that ends a keepaliveStreams call
// that hears nothing for silence with a retryable CodeControllerUnreachable
// error, for its caller (or RideOut) to call again. A stream can die
// without a word: a controller deploy resets the Durable Object relaying
// it, and Cloudflare's edge, which answers the client's HTTP/2 PINGs
// itself, keeps the client's end open, so the stream would otherwise wait
// forever (issues #20, #21).
func HearKeepalives(silence time.Duration) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if silence <= 0 || spec.StreamType != connect.StreamTypeServer || !keepaliveStreams[spec.Procedure] {
				return next(ctx, spec)
			}
			sctx, cancel := context.WithCancel(ctx)
			s, err := next(sctx, spec)
			if err != nil {
				cancel()
				return nil, err
			}
			l := &listening{ClientStream: s, ctx: ctx, cancel: cancel, silence: silence, procedure: spec.Procedure}
			l.timer = time.AfterFunc(silence, func() {
				l.silent.Store(true)
				cancel()
			})
			return l, nil
		}
	}
}

// listening is a stream whose context is canceled once it has said nothing
// for silence.
type listening struct {
	connect.ClientStream
	ctx       context.Context // the caller's
	cancel    context.CancelFunc
	timer     *time.Timer
	silence   time.Duration
	procedure string
	silent    atomic.Bool
}

func (l *listening) Receive(msg any) error {
	err := l.ClientStream.Receive(msg)
	if err == nil {
		l.timer.Reset(l.silence)
		return nil
	}
	l.stop()
	if l.silent.Load() && l.ctx.Err() == nil {
		e := Errorf(CodeControllerUnreachable, "%s said nothing for %s: the stream died on its way (such as in a controller deploy)", l.procedure, l.silence)
		e.Retryable = true
		return ToConnect(e)
	}
	return err
}

func (l *listening) Close() error {
	l.stop()
	return l.ClientStream.Close()
}

func (l *listening) stop() {
	l.timer.Stop()
	l.cancel()
}
