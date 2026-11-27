package apicmd_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/alecthomas/kong"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	hotapi "github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// giving is a test API whose commands give their params in the ways the
// options allow beyond a field's own flag: from a file, as text a hook
// reads, as a switch, as the words after --, or not at all.
const giving = `syntax = "proto3";
package give.v1;
import "hotbed/v1/hotbed.proto";
service JobService {
  rpc PutJob(PutJobRequest) returns (Job) {
    option (hotbed.v1.command) = {
      name: "job add"
      session: true
      omit: ["job.created_at"]
      params: [{field: "job.name", help: "The job's name, here.", required: true}]
    };
    option (hotbed.v1.command) = {
      name: "job draft"
      session: true
      aliases: ["sketch"]
      omit: ["job"]
      params: [{field: "note", name: "memo"}]
    };
  }
  rpc Report(ReportRequest) returns (Job) {
    option (hotbed.v1.command) = {name: "job report", input: {unless: "report"}};
  }
}
message Job {
  string name = 1;
  int64 mem_bytes = 2 [(hotbed.v1.param) = {name: "mem", parse: "Kilobytes"}];
  repeated int64 limits = 3 [(hotbed.v1.param) = {name: "limit", parse: "Kilobytes"}];
  int64 created_at = 4;
  repeated string args = 5 [(hotbed.v1.param) = {positional: true, passthrough: true}];
  int64 soft = 6 [(hotbed.v1.param) = {default: "'1K'", parse: "Kilobytes"}];
}
enum Actor {
  ACTOR_UNSPECIFIED = 0;
  ACTOR_USER = 1;
  ACTOR_AGENT = 2;
}
message PutJobRequest {
  Job job = 1 [(hotbed.v1.param) = {flatten: true}];
  string body = 2 [(hotbed.v1.param) = {file_flag: "body-file"}];
  string note = 3;
  Actor actor = 4 [(hotbed.v1.param) = {name: "as-user", switch_value: "ACTOR_USER"}];
  string session_id = 5 [(hotbed.v1.param) = {hidden: true, default: "session", unless: "actor"}];
  string remote = 6 [(hotbed.v1.param) = {hidden: true, default: "checkout_remote"}];
  string repo = 7 [(hotbed.v1.param) = {default: "env:GIVE_REPO", hook: "JobRepo"}];
}
message ReportRequest {
  repeated Job jobs = 1;
  string report = 2 [(hotbed.v1.param) = {file_flag: "from-report"}];
}
`

// givingSchema is giving's commands, their requests' Go types registered
// as hooks need them.
func givingSchema(t *testing.T) *apicmd.Schema {
	t.Helper()
	s, err := apicmd.Load(descriptors(t, map[string]string{"test.proto": giving}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Commands {
		for _, d := range []protoreflect.MessageDescriptor{c.Method.Input(), c.Method.Output()} {
			if _, err := protoregistry.GlobalTypes.FindMessageByName(d.FullName()); err != nil {
				if err := protoregistry.GlobalTypes.RegisterMessage(dynamicpb.NewMessageType(d)); err != nil && !strings.Contains(err.Error(), "conflict") {
					t.Fatal(err)
				}
			}
		}
	}
	return s
}

type session string

func (s session) Session(context.Context, string) (string, error) { return string(s), nil }
func (s session) Named(context.Context, string) (string, error)   { return string(s), nil }

// givingRunner runs giving's commands: Kilobytes reads "<n>K" as n*1024.
func givingRunner() *apicmd.Runner {
	hooks := apicmd.NewHooks()
	hooks.Parse["Kilobytes"] = func(_ context.Context, _ *apicmd.Call, text string) (any, error) {
		n, err := strconv.ParseInt(strings.TrimSuffix(text, "K"), 10, 64)
		if err != nil {
			return nil, errors.New("not like 2K")
		}
		return n * 1024, nil
	}
	hooks.Fields["JobRepo"] = func(context.Context, *apicmd.Call, proto.Message) (any, error) { return "from-hook", nil }
	return &apicmd.Runner{
		Hooks: hooks, Sessions: session("brave-otter"),
		Getenv:         func(k string) string { return map[string]string{"GIVE_REPO": ""}[k] },
		CheckoutRemote: func(context.Context) (string, error) { return "github.com/o/app", nil },
	}
}

// parse parses args as the command line does, and loads what they name.
func parse(t *testing.T, s *apicmd.Schema, stdin string, args ...string) (apicmd.Parsed, error) {
	t.Helper()
	k, err := s.Kong()
	if err != nil {
		t.Fatal(err)
	}
	parser, err := kong.New(&struct{}{}, k.Options()...)
	if err != nil {
		t.Fatal(err)
	}
	kctx, err := parser.Parse(args)
	if err != nil {
		return apicmd.Parsed{}, err
	}
	p, ok := k.Selected(kctx.Selected())
	if !ok {
		t.Fatalf("%v selected no command", args)
	}
	return p, p.Load(func(path string) ([]byte, error) {
		if path == "-" {
			return []byte(stdin), nil
		}
		return os.ReadFile(path)
	})
}

func get(m protoreflect.Message, path string) protoreflect.Value {
	var v protoreflect.Value
	for _, name := range strings.Split(path, ".") {
		f := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		v = m.Get(f)
		if f.Message() != nil && !f.IsList() {
			m = v.Message()
		}
	}
	return v
}

// A file flag gives its param the file's content, - for stdin; one empty,
// or given beside its param, is refused.
func TestFileFlag(t *testing.T) {
	s := givingSchema(t)
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("# From the file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		stdin, out string
	}{
		{[]string{"--body-file", file}, "", "# From the file\n"},
		{[]string{"--body-file", "-"}, "from stdin", "from stdin"},
		{[]string{"--body", "given"}, "", "given"},
	} {
		p, err := parse(t, s, tc.stdin, append([]string{"job", "add", "--name", "n"}, tc.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		req, _, err := givingRunner().Build(t.Context(), p.Command, p.Args)
		if err != nil {
			t.Fatal(err)
		}
		if got := get(req, "body").String(); got != tc.out {
			t.Errorf("%v: body = %q, want %q", tc.args, got, tc.out)
		}
	}
	if _, err := parse(t, s, "", "job", "add", "--name", "n", "--body", "x", "--body-file", file); err == nil || !strings.Contains(err.Error(), "give --body or --body-file, not both") {
		t.Errorf("--body with --body-file = %v, want refused", err)
	}
	if _, err := parse(t, s, " \n", "job", "add", "--name", "n", "--body-file", "-"); err == nil || !strings.Contains(err.Error(), "--body-file - is empty") {
		t.Errorf("empty --body-file = %v, want refused", err)
	}
	// MCP gives the value itself, never a file.
	for _, tool := range s.Tools() {
		if _, ok := tool.InputSchema["properties"].(map[string]any)["body_file"]; ok {
			t.Errorf("%s takes body_file in MCP", tool.Name)
		}
	}
}

// A parsed param is given as text, each of a list's values read by its
// hook, and so is its default; text the hook refuses names the flag.
func TestParsedParam(t *testing.T) {
	s := givingSchema(t)
	p, err := parse(t, s, "", "job", "add", "--name", "n", "--mem", "2K", "--limit", "1K", "--limit", "3K")
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := givingRunner().Build(t.Context(), p.Command, p.Args)
	if err != nil {
		t.Fatal(err)
	}
	limits := get(req, "job.limits").List()
	if get(req, "job.mem_bytes").Int() != 2048 || limits.Len() != 2 || limits.Get(1).Int() != 3072 || get(req, "job.soft").Int() != 1024 {
		t.Errorf("built %v, want mem 2048, limits [1024 3072], soft's default 1K read as 1024", req)
	}
	p, err = parse(t, s, "", "job", "add", "--name", "n", "--mem", "lots")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := givingRunner().Build(t.Context(), p.Command, p.Args); err == nil || !strings.Contains(err.Error(), `--mem "lots": not like 2K`) {
		t.Errorf("--mem lots = %v, want the hook's refusal naming the flag", err)
	}
	// MCP takes the text too.
	args, err := s.Command("job add").MCPArgs([]byte(`{"name": "n", "mem": "4K", "limit": ["1K"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req, _, err = givingRunner().Build(t.Context(), s.Command("job add"), args); err != nil || get(req, "job.mem_bytes").Int() != 4096 {
		t.Errorf("MCP mem 4K = %v (%v), want 4096", req, err)
	}
}

// A switch is a bool flag setting its field to its value, and a param's
// unless drops its default when the switch is given; checkout_remote is the
// runner's checkout's remote; a default that may be empty yields to a hook.
func TestSwitchUnlessAndCheckoutRemote(t *testing.T) {
	s := givingSchema(t)
	p, err := parse(t, s, "", "job", "add", "--name", "n")
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := givingRunner().Build(t.Context(), p.Command, p.Args)
	if err != nil {
		t.Fatal(err)
	}
	if get(req, "actor").Enum() != 0 || get(req, "session_id").String() != "brave-otter" || get(req, "remote").String() != "github.com/o/app" || get(req, "repo").String() != "from-hook" {
		t.Errorf("built %v, want no actor, the session, the checkout's remote, and the hook's repo past an empty env", req)
	}
	p, err = parse(t, s, "", "job", "add", "--name", "n", "--as-user")
	if err != nil {
		t.Fatal(err)
	}
	if req, _, err = givingRunner().Build(t.Context(), p.Command, p.Args); err != nil {
		t.Fatal(err)
	}
	if get(req, "actor").Enum() != 1 || get(req, "session_id").String() != "" {
		t.Errorf("built %v with --as-user, want ACTOR_USER and no session", req)
	}
	p, err = parse(t, s, "", "job", "add", "--name", "n", "--as-user=false")
	if err != nil {
		t.Fatal(err)
	}
	if req, _, err = givingRunner().Build(t.Context(), p.Command, p.Args); err != nil || get(req, "actor").Enum() != 0 || get(req, "session_id").String() != "brave-otter" {
		t.Errorf("built %v (%v) with --as-user=false, want it not given", req, err)
	}
}

// omit drops params, params renames them, changes their help and requires
// them, and aliases name a command again: each for its command alone.
func TestOmitParamsAndAliases(t *testing.T) {
	s := givingSchema(t)
	names := func(c *apicmd.Command) string {
		var out []string
		for _, p := range c.Params {
			out = append(out, p.Name)
		}
		return strings.Join(out, " ")
	}
	add, draft := s.Command("job add"), s.Command("job draft")
	if got, want := names(add), "name mem limit args soft body note as-user session-id remote repo"; got != want {
		t.Errorf("job add params = %s, want %s", got, want)
	}
	if got, want := names(draft), "body memo as-user session-id remote repo"; got != want {
		t.Errorf("job draft params = %s, want %s", got, want)
	}
	if p := add.Params[0]; !p.Required || p.Help != "The job's name, here." {
		t.Errorf("job add's name = %+v, want required, with its help", p)
	}
	if _, err := parse(t, s, "", "job", "add"); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Errorf("job add without --name = %v, want it required", err)
	}
	p, err := parse(t, s, "", "job", "sketch", "--memo", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if p.Command != draft {
		t.Errorf("job sketch ran %s, want job draft", p.Command.Name())
	}
	req, _, err := givingRunner().Build(t.Context(), p.Command, p.Args)
	if err != nil || get(req, "note").String() != "hi" || req.Has(req.Descriptor().Fields().ByName("job")) {
		t.Errorf("job sketch --memo hi = %v (%v), want note hi and no job", req, err)
	}
	if doc := apicmd.RenderCommandReference(s.Schema); !strings.Contains(doc, "also `sketch`") || !strings.Contains(doc, "| `--body-file` |") {
		t.Errorf("reference misses the alias or the file flag:\n%s", doc)
	}
}

// A passthrough param takes every word after --, flags too: the command
// line's caller hands them over (SetDashArgs in cmd/hotbed).
func TestPassthroughParam(t *testing.T) {
	s := givingSchema(t)
	p, err := parse(t, s, "", "job", "add", "--name", "n", "sh", "-c", "--not-a-flag")
	if err != nil {
		t.Fatal(err)
	}
	var args *spec.Param
	for _, q := range p.Command.Params {
		if q.Passthrough {
			args = q
		}
	}
	if got, _ := p.Args.Given[args].([]string); strings.Join(got, " ") != "sh -c --not-a-flag" {
		t.Errorf("passthrough = %q, want every word after the flags", got)
	}
}

// What stands in for the JSON, given, means the JSON is not read; given
// beside --file, it is refused.
func TestInputUnless(t *testing.T) {
	s := givingSchema(t)
	file := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(file, []byte("the report"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := parse(t, s, "", "job", "report", "--from-report", file)
	if err != nil {
		t.Fatal(err)
	}
	if p.Args.Input != nil {
		t.Errorf("JSON read beside --from-report: %q", p.Args.Input)
	}
	req, _, err := givingRunner().Build(t.Context(), p.Command, p.Args)
	if err != nil || get(req, "report").String() != "the report" {
		t.Errorf("built %v (%v), want the report alone", req, err)
	}
	if _, err := parse(t, s, `{"jobs": []}`, "job", "report", "--from-report", file, "--file", file); err == nil || !strings.Contains(err.Error(), "give the JSON (--file) or --from-report, not both") {
		t.Errorf("--from-report with --file = %v, want refused", err)
	}
	if _, err := parse(t, s, "  ", "job", "report"); err == nil || !strings.Contains(err.Error(), "no JSON on stdin") {
		t.Errorf("no report and no JSON = %v, want the JSON asked for", err)
	}
	p, err = parse(t, s, `{"jobs": [{"name": "a"}]}`, "job", "report")
	if err != nil {
		t.Fatal(err)
	}
	if req, _, err = givingRunner().Build(t.Context(), p.Command, p.Args); err != nil || get(req, "jobs").List().Len() != 1 {
		t.Errorf("built %v (%v), want the JSON's job", req, err)
	}
}

// A Caller makes its RPC's call, and the command's before and after hooks
// still run around it: a client's own way of calling skips nothing.
func TestCallerRunsBetweenHooks(t *testing.T) {
	s, err := apicmd.Load(descriptors(t, map[string]string{"test.proto": `syntax = "proto3";
package caller.v1;
import "hotbed/v1/hotbed.proto";
service PingService {
  rpc Ping(PingRequest) returns (PingRequest) {
    option (hotbed.v1.command) = {name: "ping", before: ["Before"], after: ["After"]};
  }
}
message PingRequest { string says = 1; }
`}))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Command("ping")
	d := c.Method.Input()
	if _, err := protoregistry.GlobalTypes.FindMessageByName(d.FullName()); err != nil {
		if err := protoregistry.GlobalTypes.RegisterMessage(dynamicpb.NewMessageType(d)); err != nil {
			t.Fatal(err)
		}
	}
	says := d.Fields().ByName("says")
	var order []string
	hooks := apicmd.NewHooks()
	hooks.Before["Before"] = func(_ context.Context, _ *apicmd.Call, req proto.Message) error {
		order = append(order, "before")
		req.ProtoReflect().Set(says, protoreflect.ValueOfString("hello"))
		return nil
	}
	hooks.After["After"] = func(_ context.Context, _ *apicmd.Call, _, res proto.Message) error {
		order = append(order, "after saw "+res.ProtoReflect().Get(says).String())
		return nil
	}
	// The API's own call would fail: there is no server.
	client, err := hotapi.Dial("127.0.0.1:1", hotapi.DefaultPort, http.DefaultClient, connect.ClientInterceptor(func(connect.ClientFunc) connect.ClientFunc {
		return func(context.Context, connect.Spec) (connect.ClientStream, error) {
			return nil, errors.New("the API's call was made")
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := &apicmd.Runner{Hooks: hooks, Calls: client, Waits: client, Callers: map[protoreflect.FullName]apicmd.Caller{
		c.Method.FullName(): func(_ context.Context, req proto.Message) (proto.Message, error) {
			order = append(order, "caller got "+req.ProtoReflect().Get(says).String())
			res := proto.Clone(req)
			res.ProtoReflect().Set(says, protoreflect.ValueOfString("pong"))
			return res, nil
		},
	}}
	if _, err := r.Run(t.Context(), c, apicmd.Args{}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ", "), "before, caller got hello, after saw pong"; got != want {
		t.Errorf("ran %s, want %s", got, want)
	}
}

// One hook may serve several commands: a field hook values of one type,
// seeing the request only when every use is of one; an input hook one
// message, whatever request it is of.
func TestHooksSharedAcrossRequests(t *testing.T) {
	ss, err := spec.Load(descriptors(t, map[string]string{"test.proto": `syntax = "proto3";
package share.v1;
import "hotbed/v1/hotbed.proto";
service ShareService {
  rpc A(ARequest) returns (Note) { option (hotbed.v1.command) = {name: "share a", input: {field: "note", hook: "Note"}}; }
  rpc B(BRequest) returns (Note) { option (hotbed.v1.command) = {name: "share b", input: {field: "note", hook: "Note"}}; }
  rpc C(ARequest) returns (Note) { option (hotbed.v1.command) = {name: "share c", input: {field: "note", hook: "Note"}}; }
}
message Note { string text = 1; }
message ARequest {
  Note note = 1;
  Note from = 2 [(hotbed.v1.param) = {hidden: true, hook: "From"}];
  string mine = 3 [(hotbed.v1.param) = {hidden: true, hook: "Mine"}];
}
message BRequest {
  Note note = 1;
  Note from = 2 [(hotbed.v1.param) = {hidden: true, hook: "From"}];
}
`}))
	if err != nil {
		t.Fatal(err)
	}
	if h := ss.Hooks["From"]; h.Request != nil || len(h.Uses) != 3 {
		t.Errorf("From = %+v, want it named by three commands of two requests, seeing none", h)
	}
	if h := ss.Hooks["Mine"]; h.Request == nil || h.Request.Name() != "ARequest" {
		t.Errorf("Mine = %+v, want it seeing ARequest, its one request", h)
	}
	if h := ss.Hooks["Note"]; h.Kind != spec.InputHook || len(h.Uses) != 3 {
		t.Errorf("Note = %+v, want one input hook of three commands", h)
	}
}

// Each giving option that cannot mean anything fails loading.
func TestBadGivingAnnotationsFail(t *testing.T) {
	const get = `option (hotbed.v1.command) = {name: "thing get"};`
	for _, tc := range []struct {
		name, method, fields, says string
	}{
		{"file flag on a number", get, `int32 n = 1 [(hotbed.v1.param) = {file_flag: "n-file"}];`, "file_flag is for a string flag"},
		{"file flag taken", get, `string a = 1 [(hotbed.v1.param) = {file_flag: "b"}]; string b = 2;`, "are both --b"},
		{"parsed and hidden", get, `int64 n = 1 [(hotbed.v1.param) = {parse: "Kilobytes", hidden: true, hook: "N"}];`, "is parsed and hidden"},
		{"passthrough flag", get, `repeated string args = 1 [(hotbed.v1.param) = {passthrough: true}];`, "passthrough is for a repeated string positional"},
		{"passthrough required", get, `repeated string args = 1 [(hotbed.v1.param) = {positional: true, passthrough: true, required: true}];`, "never required"},
		{"switch on a list", get, `repeated Color c = 1 [(hotbed.v1.param) = {switch_value: "COLOR_RED"}];`, "switch_value is for a flag of one value"},
		{"switch of another type", get, `Color c = 1 [(hotbed.v1.param) = {switch_value: "COLOR_BLUE"}];`, `"COLOR_BLUE" is not one of red`},
		{"unless with nothing to drop", get, `string a = 1 [(hotbed.v1.param) = {unless: "b"}]; bool b = 2;`, "unless drops its default or hook, and it has neither"},
		{"unless naming nothing", get, `string a = 1 [(hotbed.v1.param) = {default: "'x'", unless: "nope"}];`, `unless "nope" is no other param`},
		{"omit naming nothing", `option (hotbed.v1.command) = {name: "thing get", omit: ["nope"]};`, `string a = 1;`, `omit "nope" names no param`},
		{"params naming nothing", `option (hotbed.v1.command) = {name: "thing get", params: [{field: "nope", name: "x"}]};`, `string a = 1;`, `params names "nope"`},
		{"input unless naming nothing", `option (hotbed.v1.command) = {name: "thing get", input: {unless: "nope"}};`, `string a = 1;`, `input unless "nope" is no param given`},
		{"alias of two words", `option (hotbed.v1.command) = {name: "thing get", aliases: ["two words"]};`, `string a = 1;`, "is not one word"},
		{"checkout_remote of a number", get, `int32 n = 1 [(hotbed.v1.param) = {default: "checkout_remote"}];`, `"checkout_remote" is string, not int32`},
		{"parse hook of two types", get, `int64 a = 1 [(hotbed.v1.param) = {parse: "P"}]; int32 b = 2 [(hotbed.v1.param) = {parse: "P"}];`, "hook P is named by thing get too, for something else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apicmd.Load(descriptors(t, api(tc.method, tc.fields)))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Load = %v, want an error saying %q", err, tc.says)
			}
		})
	}
}
