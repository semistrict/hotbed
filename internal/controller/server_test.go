package controller_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func TestUIIsServedOnlyToTheOwner(t *testing.T) {
	store, err := controller.OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tn := tailnettest.New(ubu1)
	srv := httptest.NewServer(controller.NewServer(controller.ServerConfig{
		Store:      store,
		Identifier: tn,
		Cluster:    statedir.ControllerConfig{ClusterID: "c0ffee", Owner: "ramon@", Port: 7120},
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		UI:         http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ui:"+r.URL.Path) }),
	}).Handler())
	t.Cleanup(srv.Close)

	laptop := tn.Client(tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"})
	mallory := tn.Client(tailnet.Peer{NodeID: "nMallory", Hostname: "mallory-pc", Login: "mallory@"})
	for _, tc := range []struct {
		name   string
		client *http.Client
		path   string
		status int
		body   string
	}{
		{"owner gets the app", laptop, "/workspaces/ws-1", http.StatusOK, "ui:/workspaces/ws-1"},
		{"owner gets the root", laptop, "/", http.StatusOK, "ui:/"},
		{"other owner is refused", mallory, "/", http.StatusForbidden, ""},
		{"caller off the tailnet is refused", &http.Client{}, "/", http.StatusUnauthorized, ""},
	} {
		resp, err := tc.client.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
		if tc.body != "" && string(b) != tc.body {
			t.Errorf("%s: body %q, want %q", tc.name, b, tc.body)
		}
	}
}
