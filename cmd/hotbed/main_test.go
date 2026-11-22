package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
	"github.com/semistrict/hotbed/internal/worker"
)

var (
	ubu1   = tailnet.Self{Peer: tailnet.Peer{NodeID: "nUbu1", Hostname: "ubu1", Login: "ramon@"}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	laptop = tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
)

type output struct {
	Version string          `json:"version"`
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Data    json.RawMessage `json:"data"`
	Error   *api.Error      `json:"error"`
}

// hotbed runs the CLI with args and decodes the one envelope it prints.
func hotbed(t *testing.T, d deps, args ...string) (int, output) {
	t.Helper()
	var stdout bytes.Buffer
	d.stdout, d.stderr = &stdout, io.Discard
	code := run(t.Context(), args, d)
	var out output
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("hotbed %s printed %q, not one JSON envelope: %v", strings.Join(args, " "), stdout.String(), err)
	}
	if out.Version != "1" {
		t.Errorf("envelope version %q, want 1", out.Version)
	}
	return code, out
}

func fakeDeps() deps {
	tn := tailnettest.New(ubu1)
	return deps{tailnet: tn, http: tn.Client(laptop), stream: tn.Client(laptop), check: worker.CheckMachine}
}

func TestInitPrintsSuccessEnvelope(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hotbed")
	code, out := hotbed(t, fakeDeps(), "init", dir, "--port", "7121")
	if code != exitOK || !out.OK || out.Command != "init" {
		t.Fatalf("init: exit %d, envelope %+v", code, out)
	}
	var data controller.InitResult
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Address != "ubu1:7121" || data.Owner != "ramon@" || data.Dir != dir {
		t.Errorf("init data = %+v", data)
	}
}

func TestOutputJSONIsFormattedByDefault(t *testing.T) {
	var stdout bytes.Buffer
	d := fakeDeps()
	d.stdout, d.stderr = &stdout, io.Discard
	if code := run(t.Context(), []string{"version"}, d); code != exitOK {
		t.Fatalf("version exit %d", code)
	}
	got := stdout.String()
	if !strings.Contains(got, "\n  \"version\": \"1\",\n") || !strings.Contains(got, "\n    \"os\": ") {
		t.Errorf("expected pretty-printed JSON with 2-space indent, got:\n%s", got)
	}

	// Also verify proto.Message payloads (json.RawMessage) are indented.
	var protoBuf bytes.Buffer
	writeEnvelope(&protoBuf, success("nodes", &pb.ListNodesResponse{ClusterId: "abc"}))
	if !strings.Contains(protoBuf.String(), "\n    \"clusterId\": \"abc\"") {
		t.Errorf("expected indented protobuf JSON payload, got:\n%s", protoBuf.String())
	}
}

func TestInitTwiceIsAlreadyInitialized(t *testing.T) {
	dir := t.TempDir()
	d := fakeDeps()
	if code, out := hotbed(t, d, "init", dir); code != exitOK {
		t.Fatalf("first init: exit %d, %+v", code, out.Error)
	}
	code, out := hotbed(t, d, "init", dir)
	if code != exitError || out.OK || out.Error.Code != api.CodeAlreadyInitialized || out.Error.Retryable {
		t.Errorf("second init: exit %d, error %+v; want exit 1, ALREADY_INITIALIZED, not retryable", code, out.Error)
	}
}

func TestBadFlagIsInvalidArgs(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "init", t.TempDir(), "--no-such-flag")
	if code != exitInvalidArgs || out.OK || out.Error.Code != api.CodeInvalidArgs || out.Command != "init" {
		t.Errorf("exit %d, envelope %+v; want exit 2, INVALID_ARGS for init", code, out)
	}
}

func TestUnknownCommandIsInvalidArgs(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "frobnicate")
	if code != exitInvalidArgs || out.Error.Code != api.CodeInvalidArgs {
		t.Errorf("exit %d, error %+v; want exit 2, INVALID_ARGS", code, out.Error)
	}
}

func TestRunUninitializedDir(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "serve", t.TempDir())
	if code != exitError || out.Error.Code != api.CodeNotInitialized {
		t.Errorf("exit %d, error %+v; want exit 1, NOT_INITIALIZED", code, out.Error)
	}
}

func TestHelpPrintsGuidePointerAndExitsZero(t *testing.T) {
	var stdout bytes.Buffer
	d := fakeDeps()
	d.stdout, d.stderr = &stdout, io.Discard
	code := run(t.Context(), []string{"--help"}, d)
	if code != exitOK {
		t.Errorf("--help exit %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "hotbed skills get hotbed") {
		t.Errorf("--help does not point agents at the guide:\n%s", stdout.String())
	}
}

func TestSkillsGetHotbed(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "skills", "get", "hotbed")
	if code != exitOK || out.Command != "skills get" {
		t.Fatalf("exit %d, envelope %+v", code, out)
	}
	var data struct{ Content string }
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data.Content, "---\nname: hotbed\n") {
		t.Errorf("guide starts %.40q, want its front matter", data.Content)
	}
}

func TestSkillsGetUnknown(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "skills", "get", "nope")
	if code != exitError || out.Error.Code != api.CodeInvalidArgs {
		t.Errorf("exit %d, error %+v; want exit 1, INVALID_ARGS", code, out.Error)
	}
}

func TestNodesAgainstRunningController(t *testing.T) {
	d := fakeDeps()
	dir := t.TempDir()
	if code, out := hotbed(t, d, "init", dir); code != exitOK {
		t.Fatalf("init: exit %d, %+v", code, out.Error)
	}
	cfg, err := statedir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cluster := *cfg.Controller
	cluster.Port = 0
	c, err := controller.Listen(t.Context(), dir, cluster, d.tailnet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	code, out := hotbed(t, d, "nodes", "--controller", c.Addrs()[0].String())
	if code != exitOK {
		t.Fatalf("nodes: exit %d, %+v", code, out.Error)
	}
	data := &pb.ListNodesResponse{}
	if err := protojson.Unmarshal(out.Data, data); err != nil {
		t.Fatal(err)
	}
	if data.ClusterId != cluster.ClusterID || len(data.Nodes) != 0 {
		t.Errorf("nodes data = %v, want cluster %s with no nodes", data, cluster.ClusterID)
	}
}
