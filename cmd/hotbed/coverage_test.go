package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/worker"
)

// Every API call is something the CLI can make (AGENTS.md). The tests'
// clients record each procedure the CLI calls, and a run of the whole suite
// fails if one in the API was never called by it.

// nodeProtocol is the API workers speak to the controller on their own,
// under hotbed join and hotbed serve: no person or agent makes these calls.
var nodeProtocol = []string{
	"/hotbed.v1.ClusterService/Join",
	"/hotbed.v1.ClusterService/PushEvents",
	"/hotbed.v1.ClusterService/Heartbeat",
	// hotbed worker supervise, which a VM worker's LaunchAgent runs.
	"/hotbed.v1.ClusterService/Standby",
	"/hotbed.v1.ClusterService/ReportHostUsage",
	"/hotbed.v1.ClusterService/SaveCommandDiagnostics",
	"/hotbed.v1.ClusterService/SaveCommandSample",
	// Controller-to-worker RPC invoked over the worker tunnel by ClusterService.UpgradeSystem.
	"/hotbed.v1.WorkspaceService/UpgradeSystem",
}

var cliCalls sync.Map // procedure -> struct{}

// recording is a CLI client that notes each procedure it calls.
func recording(hc *http.Client) *http.Client {
	next := hc.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	out := *hc
	out.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		cliCalls.Store(r.URL.Path, struct{}{})
		return next.RoundTrip(r)
	})
	return &out
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// procedures is every call in the API, as Connect's paths name them.
func procedures() []string {
	var out []string
	svcs := pb.File_hotbed_v1_hotbed_proto.Services()
	for i := range svcs.Len() {
		ms := svcs.Get(i).Methods()
		for j := range ms.Len() {
			out = append(out, fmt.Sprintf("/%s/%s", svcs.Get(i).FullName(), ms.Get(j).Name()))
		}
	}
	return out
}

// uncalled is the API's calls, beyond the node protocol, that no test's CLI
// made.
func uncalled() []string {
	var out []string
	for _, p := range procedures() {
		if _, ok := cliCalls.Load(p); !ok && !slices.Contains(nodeProtocol, p) {
			out = append(out, p)
		}
	}
	return out
}

// wholeSuite says whether every test ran: only then is a call no test's CLI
// made one the CLI cannot make.
func wholeSuite() bool {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return false
		}
	}
	return true
}

func TestMain(m *testing.M) {
	worker.Main(os.Args[1:])
	code := m.Run()
	stopCFHarness()
	if code == 0 && wholeSuite() {
		if missing := uncalled(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: the CLI never made these API calls; give each a command and test it (AGENTS.md):\n  %s\n", strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

// The node protocol names calls that exist.
func TestNodeProtocolIsInTheAPI(t *testing.T) {
	all := procedures()
	for _, p := range nodeProtocol {
		if !slices.Contains(all, p) {
			t.Errorf("nodeProtocol names %s, which the API does not have", p)
		}
	}
}
