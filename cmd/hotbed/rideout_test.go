package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// flakyPeer breaks the first `broken` HTTP calls mid-request (as a restarting
// controller does) and answers subsequent calls with a Connect proto response.
type flakyPeer struct {
	broken int64
	calls  atomic.Int64
	answer proto.Message
}

func (f *flakyPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func startFlakyPeer(t *testing.T, peer *flakyPeer) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: peer}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func runGeneratedWithPeer(t *testing.T, addr, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	d := deps{
		here:         newTestHere(t, nil).here(),
		http:         &http.Client{Timeout: 10 * time.Second},
		stream:       &http.Client{},
		rideOut:      15 * time.Second,
		stdin:        strings.NewReader(stdin),
		stdout:       &stdout,
		stderr:       &stderr,
		controller:   func() (string, error) { return addr, nil },
		cache:        t.TempDir(),
		sessionsFile: t.TempDir() + "/sessions.json",
	}
	code := run(t.Context(), append([]string{"--json"}, args...), d)
	return code, stdout.String(), stderr.String()
}

// An idempotent generated CLI command (question ls) cut off mid-call by a
// controller restart must be retried by api.RideOut and succeed.
func TestRideOutRetriesIdempotentGeneratedCommand(t *testing.T) {
	t.Parallel()
	peer := &flakyPeer{
		broken: 1,
		answer: &pb.ListQuestionsResponse{
			Questions: []*pb.Question{{
				Id:    7,
				State: pb.QuestionState_QUESTION_STATE_OPEN,
				Prompt: &pb.Prompt{
					Question: "Keep retrying?",
					Header:   "Retry",
				},
			}},
		},
	}
	addr := startFlakyPeer(t, peer)
	code, stdout, stderr := runGeneratedWithPeer(t, addr, "", "question", "ls", "-p", "hotbed")
	if code != 0 {
		t.Fatalf("question ls exited %d: stdout=%q stderr=%q", code, stdout, stderr)
	}
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || !out.OK {
		t.Fatalf("question ls output=%q err=%v", stdout, err)
	}
	var res pb.ListQuestionsResponse
	if err := protojson.Unmarshal(out.Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 || res.Questions[0].Id != 7 {
		t.Errorf("got questions %v, want [id=7]", res.Questions)
	}
	if got := peer.calls.Load(); got != 2 {
		t.Errorf("peer saw %d calls, want 2 (1 dropped + 1 retry)", got)
	}
	if !strings.Contains(stderr, "out of reach") {
		t.Errorf("stderr = %q, want ride-out notice", stderr)
	}
}

// A non-idempotent generated CLI command (question add) cut off mid-call may
// have landed and must NOT be retried by api.RideOut.
func TestRideOutDoesNotRetryNonIdempotentGeneratedCommand(t *testing.T) {
	t.Parallel()
	peer := &flakyPeer{
		broken: 1,
		answer: &pb.AddQuestionsResponse{},
	}
	addr := startFlakyPeer(t, peer)
	code, stdout, stderr := runGeneratedWithPeer(t, addr, storeQuestion, "question", "add", "-s", "brave-otter")
	if code == 0 {
		t.Fatalf("question add succeeded unexpectedly: stdout=%q", stdout)
	}
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.OK || out.Error == nil {
		t.Fatalf("question add output=%q err=%v", stdout, err)
	}
	if out.Error.Code != api.CodeControllerUnreachable {
		t.Errorf("error code = %q, want %q", out.Error.Code, api.CodeControllerUnreachable)
	}
	if got := peer.calls.Load(); got != 1 {
		t.Errorf("peer saw %d calls, want 1 (must not retry non-idempotent write)", got)
	}
	if strings.Contains(stderr, "out of reach") {
		t.Errorf("stderr = %q, want no ride-out wait for non-idempotent call", stderr)
	}
}

// runCommandWithRetry recovers from a transient transport drop (e.g.
// http2: timeout awaiting response headers) without double-executing when the
// first attempt already landed on the worker.
func TestRunCommandWithRetry(t *testing.T) {
	t.Parallel()
	for _, landedFirst := range []bool{false, true} {
		name := "dropped_before_landing"
		if landedFirst {
			name = "landed_before_drop"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var runCalls, getCalls atomic.Int64
			var seenID string
			srv := startHandlerPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/RunCommand"):
					var req pb.RunCommandRequest
					if b, err := io.ReadAll(r.Body); err == nil {
						_ = proto.Unmarshal(b, &req)
					}
					if req.Id == "" {
						t.Errorf("RunCommand sent empty client ID")
					}
					n := runCalls.Add(1)
					if n == 1 {
						seenID = req.Id
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							panic(err)
						}
						conn.Close()
						return
					}
					if req.Id != seenID {
						t.Errorf("retry RunCommand ID = %q, want %q", req.Id, seenID)
					}
					writeProto(w, &pb.Command{Id: req.Id, State: pb.CommandState_COMMAND_STATE_RUNNING})
				case strings.HasSuffix(r.URL.Path, "/GetCommand"):
					getCalls.Add(1)
					if !landedFirst {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"code":"not_found","message":"no such command"}`))
						return
					}
					writeProto(w, &pb.Command{Id: seenID, State: pb.CommandState_COMMAND_STATE_RUNNING})
				default:
					http.NotFound(w, r)
				}
			}))
			rt := api.NewResilientTransport(nil)
			hc := &http.Client{Transport: rt, Timeout: 10 * time.Second}
			conn, err := api.Dial(srv, api.DefaultPort, hc)
			if err != nil {
				t.Fatal(err)
			}
			wsClient := hotbedv1connect.NewWorkspaceServiceClient(conn)
			var stderr bytes.Buffer
			d := deps{http: hc, stderr: &stderr, rideOut: 15 * time.Second}
			w := workerClients{calls: wsClient, stream: wsClient, controller: wsClient}
			cmd, err := runCommandWithRetry(t.Context(), d, w, &pb.RunCommandRequest{
				WorkspaceId: "ws1",
				Args:        []string{"echo", "hi"},
			})
			if err != nil {
				t.Fatalf("runCommandWithRetry: %v (stderr=%q)", err, stderr.String())
			}
			if cmd.Id == "" || cmd.Id != seenID {
				t.Fatalf("cmd.Id = %q, want %q", cmd.Id, seenID)
			}
			if landedFirst {
				if runCalls.Load() != 1 || getCalls.Load() != 1 {
					t.Errorf("landedFirst: runCalls=%d getCalls=%d, want 1 and 1", runCalls.Load(), getCalls.Load())
				}
			} else {
				if runCalls.Load() != 2 || getCalls.Load() != 1 {
					t.Errorf("droppedFirst: runCalls=%d getCalls=%d, want 2 and 1", runCalls.Load(), getCalls.Load())
				}
			}
			if rt.Resets() < 1 {
				t.Errorf("expected ResilientTransport reset on retry, got %d", rt.Resets())
			}
		})
	}
}

func startHandlerPeer(t *testing.T, h http.Handler) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func writeProto(w http.ResponseWriter, msg proto.Message) {
	b, err := proto.Marshal(msg)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/proto")
	_, _ = w.Write(b)
}

// Regression test for issue #18: when a FollowLog stream closes with clean EOF
// and no terminal chunk (e.g. edge/proxy idle close while a command was queued)
// and the command then exits quickly before GetCommand runs, followLogStream
// must still call drainLog so stdout/stderr is never dropped.
func TestFollowLogStreamDrainsOnCleanEOFWithoutTerminalChunk(t *testing.T) {
	t.Parallel()
	var followCalls atomic.Int64
	exitCode := int32(127)
	wantOut := "=== 1. Clippy & release build ===\nbash: line 5: cargo: command not found\n"
	srv := startHandlerPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/FollowLog"):
			n := followCalls.Add(1)
			w.Header().Set("Content-Type", "application/connect+proto")
			if n == 1 {
				// Clean EOF with zero chunks (no terminal chunk): e.g. idle
				// stream close while the command was queued.
				writeConnectStreamEnd(w)
				return
			}
			// Drain call (Follow=false): returns the full captured output.
			writeConnectStreamMsg(w, &pb.LogChunk{
				Data:       []byte(wantOut),
				NextOffset: int64(len(wantOut)),
			})
			writeConnectStreamEnd(w)
		case strings.HasSuffix(r.URL.Path, "/GetCommand"):
			writeProto(w, &pb.Command{
				Id:       "pn3yyobh",
				State:    pb.CommandState_COMMAND_STATE_FAILED,
				ExitCode: &exitCode,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	hc := &http.Client{Timeout: 10 * time.Second}
	conn, err := api.Dial(srv, api.DefaultPort, hc)
	if err != nil {
		t.Fatal(err)
	}
	wsClient := hotbedv1connect.NewWorkspaceServiceClient(conn)
	wk := workerClients{calls: wsClient, stream: wsClient, controller: wsClient}
	var out, stderr bytes.Buffer
	cmd, err := followLog(t.Context(), wk, "pn3yyobh", &out, &stderr)
	if err != nil {
		t.Fatalf("followLog: %v (stderr=%q)", err, stderr.String())
	}
	if cmd.State != pb.CommandState_COMMAND_STATE_FAILED || cmd.GetExitCode() != 127 {
		t.Fatalf("cmd = %v, want FAILED exit 127", cmd)
	}
	if out.String() != wantOut {
		t.Fatalf("captured output = %q, want %q", out.String(), wantOut)
	}
	if got := followCalls.Load(); got != 2 {
		t.Errorf("FollowLog calls = %d, want 2 (initial clean EOF + drainLog)", got)
	}
}

func writeConnectStreamMsg(w http.ResponseWriter, msg proto.Message) {
	b, err := proto.Marshal(msg)
	if err != nil {
		panic(err)
	}
	var hdr [5]byte
	hdr[0] = 0 // uncompressed data frame
	hdr[1] = byte(len(b) >> 24)
	hdr[2] = byte(len(b) >> 16)
	hdr[3] = byte(len(b) >> 8)
	hdr[4] = byte(len(b))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(b)
}

func writeConnectStreamEnd(w http.ResponseWriter) {
	end := []byte("{}")
	var hdr [5]byte
	hdr[0] = 2 // end-stream JSON frame
	hdr[4] = byte(len(end))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(end)
}

// Regression test for issue #21: when a FollowLog stream wedges silently after
// a command finishes (e.g. exit 1 with 0 lines of output on a stalled DO/edge
// stream), api.HearKeepalives ends the silent stream, and followLog finds the
// command ended, drains any output, and returns promptly.
func TestFollowLogStreamExitsWhenCommandFinishedOnWedgedStream(t *testing.T) {
	var followCalls, resets atomic.Int64
	exitCode := int32(1)
	srv := startHandlerPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/FollowLog"):
			n := followCalls.Add(1)
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if n == 1 {
				// Simulate a wedged server stream that never sends EOF.
				<-r.Context().Done()
				return
			}
			// Drain call returns clean EOF (0 lines of output).
			writeConnectStreamEnd(w)
		case strings.HasSuffix(r.URL.Path, "/GetCommand"):
			writeProto(w, &pb.Command{
				Id:       "opkxenbuxn",
				State:    pb.CommandState_COMMAND_STATE_FAILED,
				ExitCode: &exitCode,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	hc := &http.Client{Timeout: 10 * time.Second}
	conn, err := api.Dial(srv, api.DefaultPort, hc, api.HearKeepalives(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	wsClient := hotbedv1connect.NewWorkspaceServiceClient(conn)
	wk := workerClients{
		calls:       wsClient,
		stream:      wsClient,
		controller:  wsClient,
		resetStream: func() { resets.Add(1) },
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	cmd, err := followLog(ctx, wk, "opkxenbuxn", &out, &stderr)
	if err != nil {
		t.Fatalf("followLog: %v (stderr=%q)", err, stderr.String())
	}
	if cmd.State != pb.CommandState_COMMAND_STATE_FAILED || cmd.GetExitCode() != 1 {
		t.Fatalf("cmd = %v, want FAILED exit 1", cmd)
	}
	if followCalls.Load() < 2 {
		t.Errorf("FollowLog calls = %d, want >= 2 (wedged stream + drainLog)", followCalls.Load())
	}
	if resets.Load() < 1 {
		t.Errorf("resetStream calls = %d, want >= 1", resets.Load())
	}
}

// Regression test for issue #20: when a FollowLog stream for a queued command
// goes silently dead (missing keepalives, e.g. DO restart behind Cloudflare
// edge), api.HearKeepalives ends it and followLogStream reconnects before
// orphanAfter cancels the command.
func TestFollowLogStreamReconnectsWedgedSilentStream(t *testing.T) {
	var followCalls, resets atomic.Int64
	var done atomic.Bool
	exitCode := int32(0)
	srv := startHandlerPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/FollowLog"):
			n := followCalls.Add(1)
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if n == 1 {
				// First stream wedges silently with no keepalives.
				<-r.Context().Done()
				return
			}
			// Reconnected stream delivers output and terminal chunk.
			done.Store(true)
			writeConnectStreamMsg(w, &pb.LogChunk{Data: []byte("ok\n"), NextOffset: 3})
			writeConnectStreamMsg(w, &pb.LogChunk{NextOffset: 3, State: pb.CommandState_COMMAND_STATE_SUCCEEDED})
			writeConnectStreamEnd(w)
		case strings.HasSuffix(r.URL.Path, "/GetCommand"):
			state := pb.CommandState_COMMAND_STATE_QUEUED
			if done.Load() {
				state = pb.CommandState_COMMAND_STATE_SUCCEEDED
			}
			writeProto(w, &pb.Command{Id: "vlzjzcn5x7", State: state, ExitCode: &exitCode})
		default:
			http.NotFound(w, r)
		}
	}))
	hc := &http.Client{Timeout: 10 * time.Second}
	conn, err := api.Dial(srv, api.DefaultPort, hc, api.HearKeepalives(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	wsClient := hotbedv1connect.NewWorkspaceServiceClient(conn)
	wk := workerClients{
		calls:       wsClient,
		stream:      wsClient,
		controller:  wsClient,
		resetStream: func() { resets.Add(1) },
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	cmd, err := followLog(ctx, wk, "vlzjzcn5x7", &out, &stderr)
	if err != nil {
		t.Fatalf("followLog: %v (stderr=%q)", err, stderr.String())
	}
	if cmd.State != pb.CommandState_COMMAND_STATE_SUCCEEDED || out.String() != "ok\n" {
		t.Fatalf("got state=%v out=%q, want SUCCEEDED and ok\\n", cmd.State, out.String())
	}
	if followCalls.Load() != 2 {
		t.Errorf("FollowLog calls = %d, want 2 (wedged + reconnected)", followCalls.Load())
	}
	if resets.Load() < 1 {
		t.Errorf("resetStream calls = %d, want >= 1", resets.Load())
	}
}
