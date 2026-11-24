package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2/connecthttp"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/tailnet"
)

type stubTunnelService struct {
	hotbedv1connect.UnimplementedWorkspaceServiceHandler
}

func (stubTunnelService) GetWorkspace(ctx context.Context, req *pb.GetWorkspaceRequest) (*pb.Workspace, error) {
	if req.Id == "missing" {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s", req.Id).WithSuggestion("check id")
	}
	return &pb.Workspace{
		Id:   req.Id,
		Node: httpx.Peer(ctx).NodeID,
	}, nil
}

func (stubTunnelService) TraceCommand(ctx context.Context, req *pb.TraceCommandRequest, stream hotbedv1connect.WorkspaceServiceTraceCommandServerStream) error {
	if req.Id == "block" {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, line := range []string{"line-1", "line-2"} {
		if err := stream.Send(&pb.TraceCommandChunk{Text: line}); err != nil {
			return err
		}
	}
	return nil
}

func TestWorkerWebSocketTunnel(t *testing.T) {
	srv := httpx.NewServer()
	hotbedv1connect.RegisterWorkspaceServiceHandler(srv, stubTunnelService{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(httpx.WithPeer(r.Context(), tailnet.Peer{
			NodeID:   "cf:cluster-1",
			Hostname: "controller",
			Login:    "ramon",
		})))
	})

	done := make(chan error, 1)
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/worker" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sec-tok" {
			http.Error(w, "bad auth", http.StatusForbidden)
			return
		}
		if got := r.Header.Get("X-Hotbed-Node-Id"); got != "node-1" {
			http.Error(w, "bad node", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer c.CloseNow()

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		sendReq := func(msg tunnelDownMessage) {
			b, _ := json.Marshal(msg)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				t.Errorf("write %s: %v", msg.ID, err)
			}
		}
		readUp := func() tunnelUpMessage {
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				if string(data) == "ping" {
					_ = c.Write(ctx, websocket.MessageText, []byte("pong"))
					continue
				}
				var m tunnelUpMessage
				if err := json.Unmarshal(data, &m); err != nil {
					t.Fatalf("unmarshal %q: %v", data, err)
				}
				return m
			}
		}

		// 1. Unary success
		reqBytes, _ := proto.Marshal(&pb.GetWorkspaceRequest{Id: "ws-42"})
		sendReq(tunnelDownMessage{
			Type:      "rpc_req",
			ID:        "call-1",
			Procedure: "/hotbed.v1.WorkspaceService/GetWorkspace",
			Body:      base64.StdEncoding.EncodeToString(reqBytes),
			Stream:    false,
		})
		up := readUp()
		if up.Type != "rpc_msg" || up.ID != "call-1" {
			t.Fatalf("unary reply = %+v", up)
		}
		raw, err := base64.StdEncoding.DecodeString(up.Body)
		if err != nil {
			t.Fatalf("decode body: %v", err)
		}
		var ws pb.Workspace
		if err := proto.Unmarshal(raw, &ws); err != nil {
			t.Fatalf("unmarshal ws: %v", err)
		}
		if ws.Id != "ws-42" || ws.Node != "cf:cluster-1" {
			t.Fatalf("ws = %+v, want id=ws-42 node=cf:cluster-1", &ws)
		}

		// 2. Unary error preserves Hotbed ErrorInfo code + suggestion
		missBytes, _ := proto.Marshal(&pb.GetWorkspaceRequest{Id: "missing"})
		sendReq(tunnelDownMessage{
			Type:      "rpc_req",
			ID:        "call-2",
			Procedure: "/hotbed.v1.WorkspaceService/GetWorkspace",
			Body:      base64.StdEncoding.EncodeToString(missBytes),
			Stream:    false,
		})
		up = readUp()
		if up.Type != "rpc_err" || up.Code != string(api.CodeNotFound) || up.Suggestion != "check id" {
			t.Fatalf("unary err = %+v", up)
		}

		// 3. Server-streaming RPC emits each chunk then rpc_end
		traceBytes, _ := proto.Marshal(&pb.TraceCommandRequest{Id: "cmd-1"})
		sendReq(tunnelDownMessage{
			Type:      "rpc_req",
			ID:        "call-3",
			Procedure: "/hotbed.v1.WorkspaceService/TraceCommand",
			Body:      base64.StdEncoding.EncodeToString(traceBytes),
			Stream:    true,
		})
		var lines []string
		for {
			m := readUp()
			if m.ID != "call-3" {
				t.Fatalf("unexpected id %+v", m)
			}
			if m.Type == "rpc_end" {
				break
			}
			if m.Type != "rpc_msg" {
				t.Fatalf("unexpected stream frame %+v", m)
			}
			raw, _ := base64.StdEncoding.DecodeString(m.Body)
			var ch pb.TraceCommandChunk
			if err := proto.Unmarshal(raw, &ch); err != nil {
				t.Fatalf("unmarshal chunk: %v", err)
			}
			lines = append(lines, ch.Text)
		}
		if len(lines) != 2 || lines[0] != "line-1" || lines[1] != "line-2" {
			t.Fatalf("stream lines = %v, want [line-1 line-2]", lines)
		}

		// 4. rpc_cancel cancels an in-flight streaming RPC
		blockBytes, _ := proto.Marshal(&pb.TraceCommandRequest{Id: "block"})
		sendReq(tunnelDownMessage{
			Type:      "rpc_req",
			ID:        "call-4",
			Procedure: "/hotbed.v1.WorkspaceService/TraceCommand",
			Body:      base64.StdEncoding.EncodeToString(blockBytes),
			Stream:    true,
		})
		sendReq(tunnelDownMessage{Type: "rpc_cancel", ID: "call-4"})
		up = readUp()
		if up.ID != "call-4" || (up.Type != "rpc_err" && up.Type != "rpc_end") {
			t.Fatalf("cancel reply = %+v", up)
		}

		done <- nil
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsURL, err := tunnelWSURL(wsSrv.URL)
	if err != nil {
		t.Fatalf("tunnelWSURL: %v", err)
	}
	go func() {
		_ = serveTunnelOnce(ctx, handler, wsURL, "sec-tok", tailnet.Self{
			NodeID:   "node-1",
			Hostname: "ubu1",
		}, "100.73.156.10:7121", wsSrv.Client())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for tunnel test")
	}
}
