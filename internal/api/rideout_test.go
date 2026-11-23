package api_test

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// flaky is a peer whose first broken calls break under the caller, as a
// peer restarting mid-call does, and whose later ones get answer: a
// Connect unary response carrying it.
type flaky struct {
	broken int64
	calls  atomic.Int64
	answer proto.Message
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.calls.Add(1) <= f.broken {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		conn.Close()
		return
	}
	b, err := proto.Marshal(f.answer)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/proto")
	w.Write(b)
}

// serve serves h at addr, until the test ends.
func serve(t *testing.T, addr string, h http.Handler) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// cluster is a ClusterService client of addr that rides out restarts for
// window, counting the calls that started waiting.
func cluster(t *testing.T, addr string, window time.Duration) (hotbedv1connect.ClusterServiceClient, *atomic.Int64) {
	t.Helper()
	waits := &atomic.Int64{}
	conn, err := api.Dial(addr, api.DefaultPort, &http.Client{}, api.RideOut(window, func(error) { waits.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	return hotbedv1connect.NewClusterServiceClient(conn), waits
}

// An idempotent call that broke under the caller is made again until it
// is answered.
func TestRideOutRepeatsAnIdempotentCallThatBroke(t *testing.T) {
	addr := freeAddr(t)
	peer := &flaky{broken: 2, answer: &pb.Build{Number: 31}}
	serve(t, addr, peer)
	cl, waits := cluster(t, addr, time.Minute)
	b, err := cl.GetBuild(t.Context(), &pb.GetBuildRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Number != 31 || peer.calls.Load() != 3 || waits.Load() != 1 {
		t.Errorf("build %d after %d calls and %d waits, want 31 after 3 calls and 1 wait", b.Number, peer.calls.Load(), waits.Load())
	}
}

// One that is not idempotent and broke may have landed: it is not made
// again.
func TestRideOutDoesNotRepeatACallThatMayHaveLanded(t *testing.T) {
	addr := freeAddr(t)
	peer := &flaky{broken: 1, answer: &pb.JoinResponse{}}
	serve(t, addr, peer)
	cl, waits := cluster(t, addr, time.Minute)
	_, err := cl.Join(t.Context(), &pb.JoinRequest{})
	if e := api.FromConnect(err); e == nil || e.Code != api.CodeControllerUnreachable {
		t.Fatalf("Join that broke: %v, want CONTROLLER_UNREACHABLE", err)
	}
	if peer.calls.Load() != 1 || waits.Load() != 0 {
		t.Errorf("%d calls and %d waits, want 1 call and none", peer.calls.Load(), waits.Load())
	}
}

// Any call whose connection was refused never reached the peer: it is
// made again once the peer is back, idempotent or not.
func TestRideOutRepeatsAnyCallThatNeverReachedThePeer(t *testing.T) {
	addr := freeAddr(t)
	peer := &flaky{answer: &pb.JoinResponse{ClusterId: "c-1"}}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		serve(t, addr, peer)
	}()
	cl, waits := cluster(t, addr, time.Minute)
	start := time.Now()
	res, err := cl.Join(t.Context(), &pb.JoinRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClusterId != "c-1" || peer.calls.Load() != 1 || waits.Load() != 1 || time.Since(start) < time.Second {
		t.Errorf("cluster %q after %s, %d calls and %d waits, want c-1 once the peer was back, 1 call and 1 wait",
			res.ClusterId, time.Since(start), peer.calls.Load(), waits.Load())
	}
}

// A peer that stays away fails the call once the window has passed.
func TestRideOutGivesUpAfterItsWindow(t *testing.T) {
	cl, waits := cluster(t, freeAddr(t), 1500*time.Millisecond)
	start := time.Now()
	_, err := cl.ListNodes(t.Context(), &pb.ListNodesRequest{})
	if e := api.FromConnect(err); e == nil || e.Code != api.CodeControllerUnreachable {
		t.Fatalf("ListNodes of no one: %v, want CONTROLLER_UNREACHABLE", err)
	}
	if took := time.Since(start); took < 1500*time.Millisecond || took > 10*time.Second || waits.Load() != 1 {
		t.Errorf("failed after %s and %d waits, want after its 1.5s window and 1 wait", took, waits.Load())
	}
}

// A call made with WithoutRideOut fails at once, for a caller that has
// somewhere else to go.
func TestWithoutRideOutFailsAtOnce(t *testing.T) {
	cl, waits := cluster(t, freeAddr(t), time.Minute)
	start := time.Now()
	_, err := cl.ListNodes(api.WithoutRideOut(t.Context()), &pb.ListNodesRequest{})
	if e := api.FromConnect(err); e == nil || e.Code != api.CodeControllerUnreachable {
		t.Fatalf("ListNodes of no one: %v, want CONTROLLER_UNREACHABLE", err)
	}
	if took := time.Since(start); took > time.Second || waits.Load() != 0 {
		t.Errorf("failed after %s and %d waits, want at once", took, waits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// deadlineOn returns a child of parent that expires with
// context.DeadlineExceeded as soon as trigger is closed (without cancelling
// parent, matching how an HTTP client timeout aborts a single request).
func deadlineOn(parent context.Context, trigger <-chan struct{}) (context.Context, func()) {
	d := &deadlineContext{
		Context: parent,
		done:    make(chan struct{}),
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-trigger:
			err := context.DeadlineExceeded
			d.err.Store(&err)
			close(d.done)
		case <-parent.Done():
			err := parent.Err()
			d.err.Store(&err)
			close(d.done)
		case <-stop:
		}
	}()
	var once sync.Once
	return d, func() { once.Do(func() { close(stop) }) }
}

type deadlineContext struct {
	context.Context
	done chan struct{}
	err  atomic.Pointer[error]
}

func (d *deadlineContext) Deadline() (time.Time, bool) {
	if d.err.Load() != nil {
		return time.Now(), true
	}
	return d.Context.Deadline()
}

func (d *deadlineContext) Done() <-chan struct{} { return d.done }

func (d *deadlineContext) Err() error {
	if p := d.err.Load(); p != nil {
		return *p
	}
	return nil
}

// An idempotent call that timed out is not made again: the peer answered
// too slowly, and would again.
func TestRideOutDoesNotRepeatACallThatTimedOut(t *testing.T) {
	addr := freeAddr(t)
	reached := make(chan struct{})
	var reachedOnce sync.Once
	var calls atomic.Int64
	serve(t, addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		reachedOnce.Do(func() { close(reached) })
		<-r.Context().Done()
	}))
	waits := &atomic.Int64{}
	base := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(base.CloseIdleConnections)
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		ctx, cancel := deadlineOn(req.Context(), reached)
		defer cancel()
		return base.RoundTrip(req.WithContext(ctx))
	})}
	conn, err := api.Dial(addr, api.DefaultPort, hc, api.RideOut(time.Minute, func(error) { waits.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = hotbedv1connect.NewClusterServiceClient(conn).GetBuild(t.Context(), &pb.GetBuildRequest{})
	if err == nil {
		t.Fatal("GetBuild of a peer slower than the client's timeout succeeded")
	}
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("GetBuild err = %v (code %v), want DeadlineExceeded", err, connect.CodeOf(err))
	}
	if calls.Load() != 1 || waits.Load() != 0 {
		t.Errorf("%d calls and %d waits, want 1 call and none", calls.Load(), waits.Load())
	}
}
