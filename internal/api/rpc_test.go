package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

func TestBaseURLAndIsRemoteURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		remote  bool
		wantErr bool
	}{
		{in: "ubu1", want: "http://ubu1:7120", remote: false},
		{in: "ubu1:9000", want: "http://ubu1:9000", remote: false},
		{in: "https://hotbed.example.workers.dev/", want: "https://hotbed.example.workers.dev", remote: true},
		{in: "http://127.0.0.1:8787", want: "http://127.0.0.1:8787", remote: true},
		{in: "https://", wantErr: true, remote: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		if got := IsRemoteURL(tc.in); got != tc.remote {
			t.Errorf("IsRemoteURL(%q) = %v, want %v", tc.in, got, tc.remote)
		}
		got, err := ControllerBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ControllerBaseURL(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ControllerBaseURL(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ControllerBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuthInterceptorAndRoundTripper(t *testing.T) {
	var gotAuth, gotNode, gotHost, gotAddr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotNode = r.Header.Get("X-Hotbed-Node-Id")
		gotHost = r.Header.Get("X-Hotbed-Host")
		gotAddr = r.Header.Get("X-Hotbed-Addr")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Test AuthInterceptor on Connect RPC
	conn, err := Dial(srv.URL, DefaultPort, srv.Client(), AuthInterceptor("tok-1", "node-1", "ubu1", "100.1.2.3:7121"))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	cl := hotbedv1connect.NewClusterServiceClient(conn)
	_, _ = cl.ListNodes(context.Background(), &pb.ListNodesRequest{})
	if gotAuth != "Bearer tok-1" || gotNode != "node-1" || gotHost != "ubu1" || gotAddr != "100.1.2.3:7121" {
		t.Fatalf("AuthInterceptor headers = (%q, %q, %q, %q)", gotAuth, gotNode, gotHost, gotAddr)
	}

	// Test AuthRoundTripper on plain HTTP GET
	gotAuth, gotNode, gotHost, gotAddr = "", "", "", ""
	hc := &http.Client{Transport: AuthRoundTripper(srv.Client().Transport, "tok-2", "node-2", "ws2", "100.1.2.4:7121")}
	resp, err := hc.Get(srv.URL + "/builds/1/hotbed-linux-amd64")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer tok-2" || gotNode != "node-2" || gotHost != "ws2" || gotAddr != "100.1.2.4:7121" {
		t.Fatalf("AuthRoundTripper headers = (%q, %q, %q, %q)", gotAuth, gotNode, gotHost, gotAddr)
	}
}
