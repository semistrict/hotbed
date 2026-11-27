package main

import (
	"bytes"
	"encoding/json"
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
