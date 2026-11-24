package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/semistrict/hotbed/internal/httpx"
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

	// Halt the worker, wait until RideOut notices it is out of reach, then bring
	// the worker back up on the same state directory and units (the command keeps
	// running throughout).
	c.nodes[0].halt()
	c.eventually("ride-out notices worker is out of reach", func() bool {
		select {
		case code := <-done:
			t.Fatalf("hotbed run exited prematurely with code %d while worker was down (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		default:
		}
		return strings.Contains(stderr.String(), "is out of reach")
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

// Re-tests attached `hotbed run` across a real TCP listener shutdown and
// reopen on the same address (real ECONNRESET/EOF + ECONNREFUSED, matching
// `hotbed serve` restarting via execve), both when output arrived before the
// restart (r.got == true) and when the command was silent before the restart
// (r.got == false) (board 7945).
func TestAttachRidesOutRealTCPWorkerRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preOutput bool
	}{
		{"after_first_output", true},
		{"before_first_output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			c.cli.rideOut = 30 * time.Second
			c.addRepo(upstream(t))
			c.ok(nil, "session", "start", "tcp-elk")
			c.workspace("tcp-elk", nil)

			// Close the test worker's httptest server and take over its exact
			// TCP address with a real http.Server using httpx.Protocols() so we
			// can close the listener (producing real ECONNREFUSED) and reopen it.
			nd := c.nodes[0]
			addr := strings.TrimPrefix(nd.srv.URL, "http://")
			nd.srv.Close()

			listenAndServe := func(onFollow func()) (*http.Server, net.Listener) {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatalf("listen %s: %v", addr, err)
				}
				srv := &http.Server{
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if onFollow != nil && strings.HasSuffix(r.URL.Path, "/FollowLog") {
							onFollow()
						}
						(*nd.handler.Load()).ServeHTTP(w, r)
					}),
					Protocols: httpx.Protocols(),
				}
				go srv.Serve(ln)
				return srv, ln
			}

			var follow1Once sync.Once
			follow1Arrived := make(chan struct{})
			srv1, _ := listenAndServe(func() {
				follow1Once.Do(func() { close(follow1Arrived) })
			})
			var srv2 *http.Server
			t.Cleanup(func() {
				srv1.Close()
				if srv2 != nil {
					srv2.Close()
				}
			})

			gateDir := t.TempDir()
			gate1 := filepath.Join(gateDir, "resumed")
			gate2 := filepath.Join(gateDir, "release")
			var script string
			wantOut := "resumed\nafter\n"
			if tc.preOutput {
				script = fmt.Sprintf(`echo before; while [ ! -e %q ]; do sleep 0.05; done; echo resumed; while [ ! -e %q ]; do sleep 0.05; done; echo after; exit 7`, gate1, gate2)
				wantOut = "before\nresumed\nafter\n"
			} else {
				script = fmt.Sprintf(`while [ ! -e %q ]; do sleep 0.05; done; echo resumed; while [ ! -e %q ]; do sleep 0.05; done; echo after; exit 7`, gate1, gate2)
			}

			var stdout, stderr syncBuffer
			d := c.cli
			d.stdin = strings.NewReader("")
			d.stdout = &stdout
			d.stderr = &stderr

			done := make(chan int, 1)
			go func() {
				done <- run(t.Context(), []string{"run", "-s", "tcp-elk", "-r", "app", "--", "sh", "-c", script}, d)
			}()

			if tc.preOutput {
				c.eventually("pre-restart output", func() bool {
					return strings.Contains(stdout.String(), "before\n")
				})
			} else {
				var cmdID string
				c.eventually("command running before restart", func() bool {
					m := started.FindStringSubmatch(stderr.String())
					if m == nil {
						return false
					}
					cmdID = m[1]
					cmd, ok := nd.eng().Command(cmdID)
					return ok && cmd.State == pb.CommandState_COMMAND_STATE_RUNNING
				})
				select {
				case <-follow1Arrived:
				case <-time.After(10 * time.Second):
					t.Fatal("timed out waiting for FollowLog stream to attach before restart")
				}
			}

			// Abruptly kill the worker's HTTP server + engine (real TCP close + ECONNREFUSED).
			srv1.Close()
			nd.halt()

			c.eventually("ride-out reports worker out of reach", func() bool {
				select {
				case code := <-done:
					t.Fatalf("hotbed run exited %d while worker was down (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
				default:
				}
				return strings.Contains(stderr.String(), "is out of reach")
			})

			select {
			case code := <-done:
				t.Fatalf("hotbed run exited %d while worker was down (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			default:
			}

			// Restart the worker engine and reopen the TCP listener on the same address.
			nd.start()
			srv2, _ = listenAndServe(nil)

			// Release the first marker and wait deterministically until the reconnected
			// stream delivers it while the command is still running.
			if err := os.WriteFile(gate1, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			c.eventually("post-restart output streamed", func() bool {
				select {
				case code := <-done:
					t.Fatalf("hotbed run exited %d after worker restarted while waiting for post-restart output (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
				default:
				}
				return strings.Contains(stdout.String(), "resumed\n")
			})
			select {
			case code := <-done:
				t.Fatalf("hotbed run exited %d after worker restarted while command still running (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			default:
			}

			if err := os.WriteFile(gate2, nil, 0o644); err != nil {
				t.Fatal(err)
			}

			select {
			case code := <-done:
				if code != 7 {
					t.Fatalf("hotbed run exited %d, want 7 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("timed out waiting for hotbed run (stdout=%q stderr=%q)", stdout.String(), stderr.String())
			}
			if got := stdout.String(); got != wantOut {
				t.Errorf("stdout = %q, want %q", got, wantOut)
			}
		})
	}
}

// When a worker stays down past followRideOut (even when RideOut returns a
// stream whose errors surface only on Receive), followLog must not reset its
// down timer on every reconnect attempt: it must fail with WORKER_UNREACHABLE
// rather than looping forever or exiting 0 (board 7945).
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
