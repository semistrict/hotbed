package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An attached `hotbed run` must ride out a worker restart mid-command while the
// command keeps running, capture output emitted both before and after the
// restart, and exit with the command's real exit code (never 0 prematurely)
// (board 7945).
func TestAttachRidesOutWorkerRestartMidCommand(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.cli.rideOut = 30 * time.Second
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "steady-elk")
	c.workspace("steady-elk", nil)

	gateDir := t.TempDir()
	gate1 := filepath.Join(gateDir, "resumed")
	gate2 := filepath.Join(gateDir, "release")
	script := fmt.Sprintf(`echo before; while [ ! -e %q ]; do sleep 0.05; done; echo resumed; while [ ! -e %q ]; do sleep 0.05; done; echo after; exit 7`, gate1, gate2)

	var stdout, stderr syncBuffer
	d := c.cli
	d.stdin = strings.NewReader("")
	d.stdout = &stdout
	d.stderr = &stderr

	done := make(chan int, 1)
	go func() {
		done <- run(t.Context(), []string{"run", "-s", "steady-elk", "-r", "app", "--", "sh", "-c", script}, d)
	}()

	// Wait until the attached run has streamed output emitted before the restart.
	c.eventually("attached run emits pre-restart output", func() bool {
		return strings.Contains(stdout.String(), "before\n")
	})

	// Halt the worker, wait until the controller has lost its tunnel, then
	// bring the worker back up on the same state directory and units (the
	// command keeps running throughout).
	c.nodes[0].halt()
	c.eventually("the controller to lose the worker's tunnel", func() bool {
		select {
		case code := <-done:
			t.Fatalf("hotbed run exited prematurely with code %d while worker was down (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		default:
		}
		ids, err := c.store.ConnectedWorkers(t.Context())
		return err == nil && !slices.Contains(ids, c.nodes[0].peer.NodeID)
	})
	select {
	case code := <-done:
		t.Fatalf("hotbed run exited prematurely with code %d while worker was down (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	default:
	}
	c.nodes[0].start()

	// Release the first marker and wait for the reconnected FollowLog stream to
	// deliver it while the command is still running.
	if err := os.WriteFile(gate1, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c.eventually("attached run streams post-restart output", func() bool {
		select {
		case code := <-done:
			t.Fatalf("hotbed run exited prematurely with code %d after worker restart while waiting for post-restart output (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		default:
		}
		return strings.Contains(stdout.String(), "resumed\n")
	})
	select {
	case code := <-done:
		t.Fatalf("hotbed run exited prematurely with code %d after worker restart while command was still running (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	default:
	}
	var ps pb.ListCommandsResponse
	c.ok(&ps, "ps", "-s", "steady-elk")
	if len(ps.Commands) != 1 || ps.Commands[0].State != pb.CommandState_COMMAND_STATE_RUNNING {
		t.Fatalf("expected command still RUNNING after worker restart, got %+v", ps.Commands)
	}

	// Now let the command finish with exit 7.
	if err := os.WriteFile(gate2, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 7 {
			t.Fatalf("hotbed run exited %d, want 7 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for attached hotbed run to finish after worker restart (stdout=%q stderr=%q)", stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "before\nresumed\nafter\n" {
		t.Errorf("stdout = %q, want %q", got, "before\nresumed\nafter\n")
	}
}

func TestAttachFollowLogGivesUpAfterFollowRideOut(t *testing.T) {
	prev := followRideOut
	followRideOut = 2 * time.Second
	t.Cleanup(func() { followRideOut = prev })

	c := newCluster(t)
	c.cli.rideOut = 500 * time.Millisecond
	c.addRepo(upstream(t))
	c.ok(nil, "session", "start", "gone-elk")
	c.workspace("gone-elk", nil)

	gate := filepath.Join(t.TempDir(), "release")
	script := fmt.Sprintf(`echo before; while [ ! -e %q ]; do sleep 0.05; done`, gate)
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o644) })

	var stdout, stderr syncBuffer
	d := c.cli
	d.stdin = strings.NewReader("")
	d.stdout = &stdout
	d.stderr = &stderr

	done := make(chan int, 1)
	go func() {
		done <- run(t.Context(), []string{"run", "-s", "gone-elk", "-r", "app", "--", "sh", "-c", script}, d)
	}()

	c.eventually("pre-halt output", func() bool {
		return strings.Contains(stdout.String(), "before\n")
	})

	c.nodes[0].halt()

	select {
	case code := <-done:
		if code != exitError {
			t.Fatalf("hotbed run with dead worker exited %d, want %d (stdout=%q stderr=%q)", code, exitError, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), string(api.CodeWorkerUnreachable)) {
			t.Errorf("stderr = %q, want %s", stderr.String(), api.CodeWorkerUnreachable)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("followLog hung past followRideOut (stdout=%q stderr=%q)", stdout.String(), stderr.String())
	}
}

// followStatus must never exit 0 for a live, unspecified, cancelled, or failed
// command, even if a malformed response carries a 0 exit code (board 7945).
func TestAttachFollowStatusNeverExitsZeroForLiveOrFailedCommand(t *testing.T) {
	t.Parallel()
	zero := int32(0)
	seven := int32(7)

	for _, tc := range []struct {
		name string
		cmd  *pb.Command
	}{
		{"nil", nil},
		{"running_nil_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_RUNNING}},
		{"running_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_RUNNING, ExitCode: &zero}},
		{"queued_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_QUEUED, ExitCode: &zero}},
		{"parked_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_PARKED, ExitCode: &zero}},
		{"unspecified_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_UNSPECIFIED, ExitCode: &zero}},
		{"cancelled_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_CANCELLED, ExitCode: &zero, Reason: "stopped"}},
		{"failed_zero_exit", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_FAILED, ExitCode: &zero, Reason: "lost isolation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code, err := followStatus("c1", tc.cmd, &stderr)
			if err == nil && code == 0 {
				t.Fatalf("followStatus(%s) = (0, nil), must never exit 0", tc.name)
			}
			if err == nil {
				t.Fatalf("followStatus(%s) = (%d, nil), want non-nil error", tc.name, code)
			}
		})
	}

	var stderr bytes.Buffer
	if code, err := followStatus("c1", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_SUCCEEDED, ExitCode: &zero}, &stderr); err != nil || code != 0 {
		t.Errorf("SUCCEEDED(0) = (%d, %v), want (0, nil)", code, err)
	}
	if code, err := followStatus("c1", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_FAILED, ExitCode: &seven}, &stderr); err != nil || code != 7 {
		t.Errorf("FAILED(7) = (%d, %v), want (7, nil)", code, err)
	}
	if code, err := followStatus("c1", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_CANCELLED, ExitCode: &seven, Reason: "stopped"}, &stderr); err != nil || code != 7 {
		t.Errorf("CANCELLED(7) = (%d, %v), want (7, nil)", code, err)
	}
	if code, err := followStatus("c1", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_CANCELLED, Reason: "killed by signal 15"}, &stderr); err != nil || code != 143 {
		t.Errorf("CANCELLED(signal 15) = (%d, %v), want (143, nil)", code, err)
	}
	// A command whose VM ended under it never exited: not 137 as if killed,
	// but a retryable failure of its VM.
	code, err := followStatus("c1", &pb.Command{Id: "c1", State: pb.CommandState_COMMAND_STATE_FAILED, Reason: "its VM was killed by signal 9"}, &stderr)
	var ae *api.Error
	if code != 0 || !errors.As(err, &ae) || ae.Code != api.CodeWorkspaceFailed || !ae.Retryable || ae.Message != "c1 failed: its VM was killed by signal 9" {
		t.Errorf("FAILED(its VM was killed by signal 9) = (%d, %#v), want (0, retryable %s)", code, err, api.CodeWorkspaceFailed)
	}
}

type fakeWorkspaceService struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
	followLog  func(context.Context, *pb.FollowLogRequest, hotbedv1connect.WorkspaceServiceFollowLogServerStream) error
	getCommand func(context.Context, *pb.GetCommandRequest) (*pb.Command, error)
}

func (f *fakeWorkspaceService) FollowLog(ctx context.Context, req *pb.FollowLogRequest, stream hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
	return f.followLog(ctx, req, stream)
}

func (f *fakeWorkspaceService) GetCommand(ctx context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
	return f.getCommand(ctx, req)
}

// When a worker restarts and has lost a command (FollowLog returns NOT_FOUND)
// while the controller still reports COMMAND_STATE_RUNNING, followLog must fail
// promptly with NOT_FOUND instead of spinning in a tight loop. Likewise, a
// persistent retryable stream failure while GetCommand reports RUNNING must
// preserve the down timer and fail once followRideOut expires.
func TestAttachFollowLogDoesNotSpinWhenWorkerReturnsNotFoundAndControllerRunning(t *testing.T) {
	newClient := func(t *testing.T, h hotbedv1connect.WorkspaceServiceHandler) hotbedv1connect.WorkspaceServiceClient {
		t.Helper()
		cs := connect.NewServer()
		hotbedv1connect.RegisterWorkspaceServiceHandler(cs, h)
		mux := http.NewServeMux()
		connecthttp.Mount(mux, cs)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		cl, err := api.Dial(strings.TrimPrefix(srv.URL, "http://"), api.WorkerPort, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		return hotbedv1connect.NewWorkspaceServiceClient(cl)
	}

	t.Run("non-retryable NOT_FOUND fails promptly without spinning", func(t *testing.T) {
		var followCalls, workerGetCalls, ctrlGetCalls atomic.Int32
		workerCl := newClient(t, &fakeWorkspaceService{
			followLog: func(_ context.Context, _ *pb.FollowLogRequest, _ hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
				followCalls.Add(1)
				return api.ToConnect(api.Errorf(api.CodeNotFound, "no command c-lost on this node"))
			},
			getCommand: func(_ context.Context, _ *pb.GetCommandRequest) (*pb.Command, error) {
				workerGetCalls.Add(1)
				return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no command c-lost on this node"))
			},
		})
		ctrlCl := newClient(t, &fakeWorkspaceService{
			getCommand: func(_ context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
				ctrlGetCalls.Add(1)
				return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
		})

		wk := workerClients{calls: workerCl, stream: workerCl, controller: ctrlCl}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var stdout, stderr bytes.Buffer
		_, err := followLog(ctx, wk, "c-lost", &stdout, &stderr)
		if err == nil {
			t.Fatal("followLog succeeded, want NOT_FOUND")
		}
		if code := api.CodeOf(err); code != api.CodeNotFound {
			t.Fatalf("followLog err = %v (code %s), want %s", err, code, api.CodeNotFound)
		}
		if got := followCalls.Load(); got != 1 {
			t.Errorf("FollowLog called %d times, want 1 (must not spin)", got)
		}
	})

	t.Run("retryable stream failure while controller reports RUNNING obeys followRideOut", func(t *testing.T) {
		prev := followRideOut
		followRideOut = 300 * time.Millisecond
		t.Cleanup(func() { followRideOut = prev })

		var followCalls atomic.Int32
		workerCl := newClient(t, &fakeWorkspaceService{
			followLog: func(_ context.Context, _ *pb.FollowLogRequest, _ hotbedv1connect.WorkspaceServiceFollowLogServerStream) error {
				followCalls.Add(1)
				return connect.NewError(connect.CodeUnavailable, "stream unavailable")
			},
			getCommand: func(_ context.Context, _ *pb.GetCommandRequest) (*pb.Command, error) {
				return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no command c-retry on this node"))
			},
		})
		ctrlCl := newClient(t, &fakeWorkspaceService{
			getCommand: func(_ context.Context, req *pb.GetCommandRequest) (*pb.Command, error) {
				return &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_RUNNING}, nil
			},
		})

		wk := workerClients{calls: workerCl, stream: workerCl, controller: ctrlCl}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var stdout, stderr bytes.Buffer
		_, err := followLog(ctx, wk, "c-retry", &stdout, &stderr)
		if err == nil {
			t.Fatal("followLog succeeded, want unreachable error after followRideOut")
		}
		if ctx.Err() != nil {
			t.Fatalf("followLog hung until context timeout instead of stopping at followRideOut: %v", err)
		}
		if !api.FromConnect(err).Retryable {
			t.Fatalf("followLog err = %v, want retryable error", err)
		}
		if got := followCalls.Load(); got > 3 {
			t.Errorf("FollowLog called %d times within %v, expected bounded retries", got, followRideOut)
		}
	})
}
