package controller_test

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

var (
	ubu1  = tailnet.Self{Peer: tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	asahi = tailnet.Peer{NodeID: "nAsahi", Hostname: "asahimbp", Login: "ramon@"}
)

func TestInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hotbed")
	res, err := controller.Init(t.Context(), dir, 7121, tailnettest.New(ubu1))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(res.ClusterID) {
		t.Errorf("cluster ID %q is not 16 hex digits", res.ClusterID)
	}
	want := controller.InitResult{
		Dir:     dir,
		Owner:   "ramon@",
		Address: "ubu1:7121",
		Next: []string{
			"on this machine: hotbed serve " + dir,
			"on each worker: hotbed join ubu1:7121 DIR, then sudo hotbed serve DIR",
		},
	}
	if diff := cmp.Diff(want, res, cmpopts.IgnoreFields(controller.InitResult{}, "ClusterID")); diff != "" {
		t.Errorf("Init (-want +got):\n%s", diff)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantCfg := statedir.Config{Controller: &statedir.ControllerConfig{ClusterID: res.ClusterID, Owner: "ramon@", Port: 7121}}
	if diff := cmp.Diff(wantCfg, cfg); diff != "" {
		t.Errorf("config (-want +got):\n%s", diff)
	}
}

func TestInitDefaultPortAddressIsHostname(t *testing.T) {
	res, err := controller.Init(t.Context(), t.TempDir(), api.DefaultPort, tailnettest.New(ubu1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "ubu1" {
		t.Errorf("address = %q, want ubu1", res.Address)
	}
}

func TestInitRefusesInitializedDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := controller.Init(t.Context(), dir, api.DefaultPort, tailnettest.New(ubu1)); err != nil {
		t.Fatal(err)
	}
	_, err := controller.Init(t.Context(), dir, api.DefaultPort, tailnettest.New(ubu1))
	if !errors.Is(err, statedir.ErrAlreadyInitialized) {
		t.Errorf("second Init = %v, want ErrAlreadyInitialized", err)
	}
}

func TestInitRejectsBadPort(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		_, err := controller.Init(t.Context(), t.TempDir(), port, tailnettest.New(ubu1))
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeInvalidArgs {
			t.Errorf("Init with port %d = %v, want INVALID_ARGS", port, err)
		}
	}
}

// serve runs the controller in dir on a free loopback port and returns its
// address and a stop function, which the test's cleanup also calls.
func serve(t *testing.T, dir string, tn *tailnettest.Fake) (string, func()) {
	t.Helper()
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cluster := *cfg.Controller
	cluster.Port = 0
	c, err := controller.Listen(t.Context(), dir, cluster, tn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Serve: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return c.Addrs()[0].String(), stop
}

func TestNodesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	tn := tailnettest.New(ubu1)
	if _, err := controller.Init(t.Context(), dir, api.DefaultPort, tn); err != nil {
		t.Fatal(err)
	}

	addr, stop := serve(t, dir, tn)
	first, _, err := api.Controller(addr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Join(t.Context(), &pb.JoinRequest{}); err != nil {
		t.Fatal(err)
	}
	stop()

	addr, _ = serve(t, dir, tn)
	second, _, err := api.Controller(addr, tn.Client(asahi))
	if err != nil {
		t.Fatal(err)
	}
	res, err := second.ListNodes(t.Context(), &pb.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Hostname != "asahimbp" {
		t.Errorf("nodes after restart = %+v, want asahimbp", res.Nodes)
	}
}
