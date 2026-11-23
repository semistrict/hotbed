package api

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
)

// RideOut is a client interceptor that waits out its peer's restart, such
// as a deploy's: a call that cannot reach the peer is made again, once a
// second, for up to window from its first failure, when making it again
// is safe: the call never reached the peer (its connection was refused),
// or its method is idempotent (proto idempotency_level) and no answer had
// come yet. waiting is called when a call starts waiting, with why.
//
// It covers unary and server-streaming calls; a stream that breaks after
// its first message is the caller's to pick up, as is every call made with
// a context from WithoutRideOut.
func RideOut(window time.Duration, waiting func(err error)) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if window <= 0 || ctx.Value(noRideOut{}) != nil ||
				(spec.StreamType != connect.StreamTypeUnary && spec.StreamType != connect.StreamTypeServer) {
				return next(ctx, spec)
			}
			s, err := next(ctx, spec)
			if err != nil {
				return nil, err
			}
			return &replaying{ctx: ctx, spec: spec, next: next, cur: s, window: window, waiting: waiting}, nil
		}
	}
}

type noRideOut struct{}

// WithoutRideOut is ctx for calls that fail at once when the peer is out of
// reach, for a caller that has somewhere else to go then.
func WithoutRideOut(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRideOut{}, true)
}

// replaying is a call that RideOut may make again: it keeps what was sent,
// to send it again on a new stream.
type replaying struct {
	ctx     context.Context
	spec    connect.Spec
	next    connect.ClientFunc
	window  time.Duration
	waiting func(err error)

	// mu guards cur and done, for Close, which may come at any time.
	mu   sync.Mutex
	cur  connect.ClientStream
	done bool // closed: no new stream is opened

	sent   []any
	closed bool // the send side is closed
	// broken is set when sending on cur failed: Receive then says why.
	broken bool
	got    bool      // an answer came: the call is not made again past it
	since  time.Time // of the first failure
}

func (r *replaying) stream() connect.ClientStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

func (r *replaying) SendHeaders() error {
	if !r.broken && r.stream().SendHeaders() != nil {
		r.broken = true
	}
	return nil
}

func (r *replaying) Send(msg any) error {
	r.sent = append(r.sent, msg)
	if !r.broken && r.stream().Send(msg) != nil {
		r.broken = true
	}
	return nil
}

func (r *replaying) CloseSend() error {
	r.closed = true
	if !r.broken && r.stream().CloseSend() != nil {
		r.broken = true
	}
	return nil
}

func (r *replaying) Receive(msg any) error {
	for {
		err := r.stream().Receive(msg)
		if err == nil {
			r.got = true
			return nil
		}
		if r.got || errors.Is(err, io.EOF) || r.ctx.Err() != nil || !r.again(err) {
			return err
		}
		if r.since.IsZero() {
			r.since = time.Now()
			r.waiting(err)
		}
		if time.Since(r.since) > r.window {
			return err
		}
		r.stream().Close()
		select {
		case <-r.ctx.Done():
			return err
		case <-time.After(time.Second):
		}
		cur, oerr := r.next(r.ctx, r.spec)
		if oerr != nil {
			return oerr
		}
		r.mu.Lock()
		if r.done {
			r.mu.Unlock()
			cur.Close()
			return err
		}
		r.cur, r.broken = cur, false
		r.mu.Unlock()
		for _, m := range r.sent {
			if cur.Send(m) != nil {
				r.broken = true
				break
			}
		}
		if r.closed && !r.broken && cur.CloseSend() != nil {
			r.broken = true
		}
	}
}

func (r *replaying) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	return r.cur.Close()
}

// again says whether a call that failed with err is safe to make again: it
// never reached the peer, or it is idempotent and failed for want of the
// peer, not for taking too long, which it would only do again.
func (r *replaying) again(err error) bool {
	if NeverSent(err) {
		return true
	}
	e := FromConnect(err)
	return e.Code == CodeControllerUnreachable && e.Retryable &&
		connect.CodeOf(err) != connect.CodeDeadlineExceeded &&
		r.spec.IdempotencyLevel != connect.IdempotencyUnknown
}

// NeverSent says whether err is a call that never reached its peer: the
// connection to it could not be made.
func NeverSent(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
