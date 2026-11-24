package controller_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestMigrateControllerExportsAndRedirectsWorkers(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "hotbed.db")
	store, err := controller.OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tn := tailnettest.New(ubu1)
	owner := tailnet.Peer{NodeID: "nMac", Hostname: "mac", Login: "ramon@"}
	worker1 := tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}
	worker2 := tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}

	// Mock target controller /admin/import endpoint (as served by cf-controller).
	var gotAuth string
	var gotPayload struct {
		ClusterID string                      `json:"clusterId"`
		Force     bool                        `json:"force"`
		Tables    map[string][]map[string]any `json:"tables"`
	}
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/import" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotPayload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			"clusterId":        gotPayload.ClusterID,
			"controllerNodeId": "cf:" + gotPayload.ClusterID,
			"importedTables":   len(gotPayload.Tables),
			"importedRows":     4,
			"workerTokens": map[string]string{
				worker1.NodeID: "hbw_tok_ubu1",
			},
		})
	}))
	defer targetSrv.Close()

	srv := controller.NewServer(controller.ServerConfig{
		Store:       store,
		Identifier:  tn,
		Cluster:     statedir.ControllerConfig{ClusterID: "cid-ubu1", Owner: "ramon@", Port: 7120},
		Self:        []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		GitAuthHTTP: targetSrv.Client(),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	ownerCluster, _, err := api.Controller(addr, tn.Client(owner))
	if err != nil {
		t.Fatalf("owner dial: %v", err)
	}
	ownerConn, err := api.Dial(addr, api.DefaultPort, tn.Client(owner))
	if err != nil {
		t.Fatalf("ownerConn: %v", err)
	}
	ownerSessions := hotbedv1connect.NewSessionServiceClient(ownerConn)

	w1Cluster, _, err := api.Controller(addr, tn.Client(worker1))
	if err != nil {
		t.Fatalf("w1 dial: %v", err)
	}
	w2Cluster, _, err := api.Controller(addr, tn.Client(worker2))
	if err != nil {
		t.Fatalf("w2 dial: %v", err)
	}

	// Join two workers and seed a repo/project before migrating.
	if _, err := w1Cluster.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatalf("w1 Join: %v", err)
	}
	if _, err := w2Cluster.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Fatalf("w2 Join: %v", err)
	}
	if _, err := ownerCluster.PutRepo(ctx, &pb.PutRepoRequest{
		Repo: &pb.Repo{Id: "hotbed", Url: "https://github.com/semistrict/hotbed.git", Branch: "main"},
	}); err != nil {
		t.Fatalf("PutRepo: %v", err)
	}

	// Migrate to targetSrv!
	migRes, err := ownerCluster.MigrateController(ctx, &pb.MigrateControllerRequest{
		TargetController: targetSrv.URL,
		TargetToken:      "owner-secret",
		WorkerToken:      "fallback-worker-tok",
		Force:            true,
	})
	if err != nil {
		t.Fatalf("MigrateController: %v", err)
	}
	if gotAuth != "Bearer owner-secret" {
		t.Fatalf("target auth = %q, want Bearer owner-secret", gotAuth)
	}
	if gotPayload.ClusterID != "cid-ubu1" || !gotPayload.Force {
		t.Fatalf("import payload = %+v", gotPayload)
	}
	if len(gotPayload.Tables["nodes"]) != 2 || len(gotPayload.Tables["repos"]) != 1 || len(gotPayload.Tables["projects"]) != 1 {
		t.Fatalf("exported tables counts: nodes=%d repos=%d projects=%d",
			len(gotPayload.Tables["nodes"]), len(gotPayload.Tables["repos"]), len(gotPayload.Tables["projects"]))
	}
	if migRes.ClusterId != "cid-ubu1" || migRes.ControllerNodeId != "cf:cid-ubu1" || migRes.MintedWorkerTokens != 1 {
		t.Fatalf("MigrateController response = %+v", migRes)
	}

	// Worker 1 receives its minted per-worker token on Heartbeat & PushEvents.
	hb1, err := w1Cluster.Heartbeat(ctx, &pb.HeartbeatRequest{ClusterId: "cid-ubu1"})
	if err != nil {
		t.Fatalf("w1 Heartbeat: %v", err)
	}
	if hb1.MigratedTo == nil || hb1.MigratedTo.Controller != targetSrv.URL || hb1.MigratedTo.Token != "hbw_tok_ubu1" || hb1.MigratedTo.ControllerNodeId != "cf:cid-ubu1" {
		t.Fatalf("w1 Heartbeat MigratedTo = %+v", hb1.MigratedTo)
	}
	push1, err := w1Cluster.PushEvents(ctx, &pb.PushEventsRequest{ClusterId: "cid-ubu1", Epoch: "e1"})
	if err != nil {
		t.Fatalf("w1 PushEvents: %v", err)
	}
	if push1.MigratedTo == nil || push1.MigratedTo.Controller != targetSrv.URL || push1.MigratedTo.Token != "hbw_tok_ubu1" {
		t.Fatalf("w1 PushEvents MigratedTo = %+v", push1.MigratedTo)
	}

	// Worker 2 (without a per-worker token in mock response) receives fallback worker token.
	hb2, err := w2Cluster.Heartbeat(ctx, &pb.HeartbeatRequest{ClusterId: "cid-ubu1"})
	if err != nil {
		t.Fatalf("w2 Heartbeat: %v", err)
	}
	if hb2.MigratedTo == nil || hb2.MigratedTo.Token != "fallback-worker-tok" {
		t.Fatalf("w2 Heartbeat MigratedTo = %+v", hb2.MigratedTo)
	}

	// Mutating client RPCs are refused once migrated, while read-only ListNodes still answers.
	if _, err := ownerSessions.StartSession(ctx, &pb.StartSessionRequest{Project: "hotbed"}); api.CodeOf(err) != api.CodeConflict {
		t.Fatalf("StartSession after migration err = %v, want CONFLICT", err)
	}
	if _, err := ownerCluster.ListNodes(ctx, &pb.ListNodesRequest{}); err != nil {
		t.Fatalf("ListNodes after migration: %v", err)
	}

	// Reopening the SQLite store preserves migrated state across controller restarts.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := controller.OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen OpenStore: %v", err)
	}
	defer reopened.Close()
	st, err := reopened.GetControllerMigration(ctx)
	if err != nil || st == nil || st.TargetController != targetSrv.URL || st.WorkerTokens[worker1.NodeID] != "hbw_tok_ubu1" {
		t.Fatalf("persisted MigrationState = %+v, err=%v", st, err)
	}
}
