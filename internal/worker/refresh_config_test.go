package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
)

type fakeConfigCluster struct {
	hotbedv1connect.UnimplementedClusterServiceHandler
	effective *pb.WorkerConfig
	asked     string
}

func (f *fakeConfigCluster) GetWorkerConfig(_ context.Context, req *pb.GetWorkerConfigRequest) (*pb.GetWorkerConfigResponse, error) {
	f.asked = req.GetNode()
	return &pb.GetWorkerConfigResponse{Effective: f.effective}, nil
}

// A worker whose cached controller config enables no runtime cannot serve,
// and so never heartbeats to learn better: it asks the controller instead,
// and caches what it is told as a heartbeat would.
func TestRefreshControllerConfigTurnsOnARuntime(t *testing.T) {
	dir := t.TempDir()
	off := false
	if err := SaveControllerConfig(dir, &pb.WorkerConfig{CladefsFuse: &pb.WorkerCladefsFUSEConfig{Enabled: &off}}); err != nil {
		t.Fatal(err)
	}
	cached, err := LoadControllerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeEnabled(MergeWorkerConfig(statedir.WorkerConfig{}, cached)) {
		t.Fatal("the cached config enables a runtime; the test needs one that does not")
	}

	on := true
	fake := &fakeConfigCluster{effective: &pb.WorkerConfig{Revision: 4, Vm: &pb.WorkerVMConfig{Enabled: &on}}}
	srv := connect.NewServer()
	hotbedv1connect.RegisterClusterServiceHandler(srv, fake)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	cluster, _, err := api.Controller(ts.URL, ts.Client())
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := refreshControllerConfig(t.Context(), cluster, dir, "vm1")
	if err != nil {
		t.Fatalf("refreshControllerConfig: %v", err)
	}
	if fake.asked != "vm1" {
		t.Fatalf("asked the controller for %q's config, want vm1's", fake.asked)
	}
	if !runtimeEnabled(MergeWorkerConfig(statedir.WorkerConfig{}, fresh)) {
		t.Fatal("the refreshed config enables no runtime")
	}
	saved, err := LoadControllerConfig(dir)
	if err != nil || saved.GetRevision() != 4 {
		t.Fatalf("cached config = (%v, %v), want revision 4", saved, err)
	}
}
