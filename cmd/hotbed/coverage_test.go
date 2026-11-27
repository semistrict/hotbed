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

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

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
	"/hotbed.v1.ClusterService/SaveCommandDiagnostics",
	"/hotbed.v1.ClusterService/SaveCommandSample",
	"/hotbed.v1.ClusterService/PushCommandOutput",
	"/hotbed.v1.ClusterService/PushDaemonLogs",
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

// procedures is every call in the API, as Connect's paths name them: every
// service of package hotbed.v1, in whichever of its files.
func procedures() []string {
	var out []string
	for _, svc := range apiServices() {
		ms := svc.Methods()
		for j := range ms.Len() {
			out = append(out, fmt.Sprintf("/%s/%s", svc.FullName(), ms.Get(j).Name()))
		}
	}
	return out
}

// apiServices is every service of package hotbed.v1, across its files.
func apiServices() []protoreflect.ServiceDescriptor {
	_ = pb.File_hotbed_v1_hotbed_proto // the package's files are registered
	var out []protoreflect.ServiceDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage("hotbed.v1", func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			out = append(out, f.Services().Get(i))
		}
		return true
	})
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
