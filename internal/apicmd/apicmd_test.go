package apicmd_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/alecthomas/kong"
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	hotapi "github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1cli"
)

// descriptors compiles test.proto, which may import hotbed/v1/hotbed.proto
// for its options, into a FileDescriptorSet with source info, as buf
// builds hotbed's own.
func descriptors(t *testing.T, srcs map[string]string) []byte {
	t.Helper()
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{"../../proto", "."},
			Accessor: func(path string) (io.ReadCloser, error) {
				if src, ok := srcs[path]; ok {
					return io.NopCloser(strings.NewReader(src)), nil
				}
				return os.Open(path)
			},
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := c.Compile(t.Context(), "test.proto")
	if err != nil {
		t.Fatalf("compile test.proto: %v", err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(f protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		for i := range f.Imports().Len() {
			add(f.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	add(files[0])
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// api is a test API's proto: a service whose one method is annotated as
// method says, with request fields fields.
func api(method, fields string) map[string]string {
	return map[string]string{"test.proto": `syntax = "proto3";
package t.v1;
import "hotbed/v1/hotbed.proto";
service ThingService {
  // Things.
  rpc GetThing(GetThingRequest) returns (Thing) {
    ` + method + `
  }
  rpc PutThing(PutThingRequest) returns (Thing) {
    option (hotbed.v1.command) = {name: "thing put"};
  }
}
message Thing { string id = 1; }
message PutThingRequest { string id = 1; }
message GetThingRequest {
  ` + fields + `
}
enum Color {
  COLOR_UNSPECIFIED = 0;
  COLOR_RED = 1;
}
`}
}

// hotbed.proto's own commands load, their CEL compiles to their fields'
// types, and the client's hooks are every hook they name.
func TestHotbedCommandsLoad(t *testing.T) {
	s, err := apicmd.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	ls := s.Command("question ls")
	if ls == nil {
		t.Fatal("no command question ls")
	}
	if ls.Help == "" || ls.Tool() != "question_ls" || !ls.Session || !ls.ReadOnly {
		t.Errorf("question ls = %+v", ls.Command)
	}
	if got := s.Groups["question"]; !strings.HasPrefix(got, "Questions agents ask the user") {
		t.Errorf("question's help = %q, want QuestionService's first sentence", got)
	}
	if got := s.Groups["project"]; !strings.HasPrefix(got, "Projects:") {
		t.Errorf("project's help = %q, want ClusterService's project group help", got)
	}
	if got := s.Groups["repo"]; !strings.HasPrefix(got, "Repos the cluster keeps built bases of") {
		t.Errorf("repo's help = %q, want ClusterService's repo group help", got)
	}
	if got := s.Groups["node"]; !strings.HasPrefix(got, "Inspect or retire individual cluster worker nodes") {
		t.Errorf("node's help = %q, want ClusterService's node group help", got)
	}
	// Migrated commands use crisp user-facing help (no Go RPC symbol prefixes).
	for _, tc := range []struct {
		cmd, badPrefix string
	}{
		{"nodes", "ListNodes"},
		{"snapshot", "GetSnapshot"},
		{"project add", "AddProject"},
		{"project rm-repo", "RemoveProjectRepos"},
		{"project ls", "ListProjects"},
		{"project show", "GetProject"},
		{"project rebuild", "RebuildProject"},
		{"project hosts", "SetProjectHosts"},
		{"repo add", "PutRepo"},
		{"repo ls", "ListRepos"},
		{"repo rm", "DeleteRepo"},
		{"ps", "ListCommands"},
	} {
		c := s.Command(tc.cmd)
		if c == nil {
			t.Fatalf("missing command %q", tc.cmd)
		}
		if strings.HasPrefix(c.Help, tc.badPrefix) {
			t.Errorf("%s help starts with Go symbol %q: %q", tc.cmd, tc.badPrefix, c.Help)
		}
	}
	if c := s.Command("project add"); !strings.Contains(c.Help, "--shared") {
		t.Errorf("project add help missing --shared guidance: %q", c.Help)
	}
}

// A command's call is as idempotent as its RPC says, so a client makes an
// idempotent one again across its peer's restart (api.RideOut) rather than
// fail; one made as idempotency unknown never is.
func TestCommandsCallAsIdempotentAsTheirRPC(t *testing.T) {
	s, err := apicmd.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	idempotent := 0
	for _, c := range s.Commands {
		want := c.Method.Options().(*descriptorpb.MethodOptions).GetIdempotencyLevel()
		if got := apicmd.Spec(c).IdempotencyLevel; int32(got) != int32(want) {
			t.Errorf("%s calls as idempotency %d, want its RPC's %s", c.Name(), got, want)
		}
		if want == descriptorpb.MethodOptions_IDEMPOTENT {
			idempotent++
		}
	}
	if idempotent == 0 {
		t.Error("no command's RPC is idempotent: the test checks nothing")
	}
}

// Each annotation that cannot mean anything fails loading, naming what is
// wrong, rather than becoming a command that misbehaves.
func TestBadAnnotationsFail(t *testing.T) {
	const get = `option (hotbed.v1.command) = {name: "thing get"};`
	const getSession = `option (hotbed.v1.command) = {name: "thing get", session: true};`
	for _, tc := range []struct {
		name, method, fields, says string
	}{
		{"default and hook", get, `string id = 1 [(hotbed.v1.param) = {default: "'x'", hook: "Id"}];`, "has a default and a hook"},
		{"hidden with nothing to fill it", get, `string id = 1 [(hotbed.v1.param) = {hidden: true}];`, "hidden with neither default nor hook"},
		{"hidden positional", get, `string id = 1 [(hotbed.v1.param) = {hidden: true, positional: true, default: "'x'"}];`, "it is never given"},
		{"default of another type", get, `string id = 1 [(hotbed.v1.param) = {default: "1"}];`, `"1" is int, not string`},
		{"default naming nothing", get, `string id = 1 [(hotbed.v1.param) = {default: "nope"}];`, `invalid string default "nope"`},
		{"bad env default", get, `string id = 1 [(hotbed.v1.param) = {default: "env:123"}];`, `invalid env variable name`},
		{"session in a command with none", get, `string id = 1 [(hotbed.v1.param) = {default: "session"}];`, "reads the session, but the command acts as none"},
		{"message given no way", get, `Thing thing = 1;`, "is a message: flatten it, take it as the command's input, or hide it"},
		{"flatten a string", get, `string id = 1 [(hotbed.v1.param) = {flatten: true}];`, "only a message field flattens"},
		{"every on a string", get, `repeated string ids = 1 [(hotbed.v1.param) = {every: "all"}];`, "every is for a repeated enum"},
		{"two flags of a name", getSession, `string session = 1;`, "are both --session"},
		{"two of a short", get, `string a = 1 [(hotbed.v1.param) = {short: "x"}]; string b = 2 [(hotbed.v1.param) = {short: "x"}];`, "-x is taken"},
		{"hook not a Go name", get, `string id = 1 [(hotbed.v1.param) = {hook: "thing_id"}];`, `hook "thing_id" is not an exported Go name`},
		{"fixed field that is not there", `option (hotbed.v1.command) = {name: "thing get", fixed: [{field: "nope", value: "1"}]};`, `string id = 1;`, `has no field "nope"`},
		{"fixed of another type", `option (hotbed.v1.command) = {name: "thing get", fixed: [{field: "id", value: "1"}]};`, `string id = 1;`, `"1" is int, not string`},
		{"input that is no message", `option (hotbed.v1.command) = {name: "thing get", input: {field: "id"}};`, `string id = 1;`, "input id is not a message"},
		{"a command's name twice", `option (hotbed.v1.command) = {name: "thing put"};`, `string id = 1;`, `a second command "thing put"`},
		{"a command and a group", `option (hotbed.v1.command) = {name: "thing"};`, `string id = 1;`, `"thing" is a command and a group of commands`},
		{"no name", `option (hotbed.v1.command) = {session: true};`, `string id = 1;`, "a command needs a name"},
		{"help's short", get, `string id = 1 [(hotbed.v1.param) = {short: "h"}];`, "-h is help's"},
		{"bytes as a flag", get, `bytes data = 1;`, "bytes are given as no flag"},
		{"a message in a flattened one", `option (hotbed.v1.command) = {name: "thing get", input: {}};`, `Wrap wrap = 1 [(hotbed.v1.param) = {flatten: true}]; } message Wrap { Thing thing = 1;`, "a message in a flattened message is given no way"},
		{"local and hidden", get, `string id = 1 [(hotbed.v1.param) = {local: true, hidden: true, default: "'x'"}];`, "is local and hidden"},
		{"local in a flattened message", get, `Wrap wrap = 1 [(hotbed.v1.param) = {flatten: true}]; } message Wrap { bool dry = 1 [(hotbed.v1.param) = {local: true}];`, "only a request's own field is local"},
		{"local in a oneof", get, `oneof which { bool dry = 1 [(hotbed.v1.param) = {local: true}]; bool wet = 2; }`, "is local in oneof which"},
		{"local and fixed", `option (hotbed.v1.command) = {name: "thing get", fixed: [{field: "dry", value: "true"}]};`, `bool dry = 1 [(hotbed.v1.param) = {local: true}];`, "is local and fixed"},
		{"fixed in a oneof", `option (hotbed.v1.command) = {name: "thing get", fixed: [{field: "dry", value: "true"}]};`, `oneof which { bool dry = 1; bool wet = 2; }`, "fixed dry is in oneof which"},
		{"required and hidden", get, `string id = 1 [(hotbed.v1.param) = {required: true, hidden: true, default: "'x'"}];`, "is required and hidden"},
		{"required with default", get, `string id = 1 [(hotbed.v1.param) = {required: true, default: "'x'"}];`, "is required with a default or hook"},
		{"required in a oneof", get, `oneof which { string a = 1 [(hotbed.v1.param) = {required: true}]; string b = 2; }`, "is required in oneof which"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apicmd.Load(descriptors(t, api(tc.method, tc.fields)))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Load = %v, want an error saying %q", err, tc.says)
			}
		})
	}
}

// A hook is one Go method: two commands naming it for different things
// would make it two, so loading fails.
func TestHookNamedForTwoThingsFails(t *testing.T) {
	srcs := api(`option (hotbed.v1.command) = {name: "thing get"};`, `string id = 1 [(hotbed.v1.param) = {hook: "Id"}];`)
	srcs["test.proto"] = strings.Replace(srcs["test.proto"], `message PutThingRequest { string id = 1; }`,
		`message PutThingRequest { int32 id = 1 [(hotbed.v1.param) = {hook: "Id"}]; }`, 1)
	_, err := apicmd.Load(descriptors(t, srcs))
	if err == nil || !strings.Contains(err.Error(), "hook Id is named by thing get too, for something else") {
		t.Errorf("Load = %v, want the hook named twice", err)
	}
}

// An option this client does not know, from a newer API's descriptors —
// whether on CommandSpec/ParamSpec/GroupSpec itself or nested inside
// CommandSpec.input or CommandSpec.fixed — fails loading and says to upgrade,
// rather than being silently ignored.
func TestNewerOptionsFail(t *testing.T) {
	for _, tc := range []struct {
		name string
		srcs map[string]string
	}{
		{
			name: "top-level command option",
			srcs: map[string]string{
				"newer.proto": `syntax = "proto3";
package hotbed.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { repeated CommandSpec command = 50000; }
message CommandSpec { string name = 1; bool sparkles = 99; }
`,
				"test.proto": `syntax = "proto3";
package t.v1;
import "newer.proto";
service ThingService {
  rpc GetThing(Thing) returns (Thing) { option (hotbed.v1.command) = {name: "thing get", sparkles: true}; }
}
message Thing { string id = 1; }
`,
			},
		},
		{
			name: "nested unknown field in CommandSpec.input",
			srcs: map[string]string{
				"newer.proto": `syntax = "proto3";
package hotbed.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { repeated CommandSpec command = 50000; }
message CommandSpec { string name = 1; InputSpec input = 6; }
message InputSpec { string field = 1; string hook = 2; bool strict = 99; }
`,
				"test.proto": `syntax = "proto3";
package t.v1;
import "newer.proto";
service ThingService {
  rpc GetThing(Thing) returns (Thing) { option (hotbed.v1.command) = {name: "thing get", input: {strict: true}}; }
}
message Thing { string id = 1; }
`,
			},
		},
		{
			name: "nested unknown field in CommandSpec.fixed",
			srcs: map[string]string{
				"newer.proto": `syntax = "proto3";
package hotbed.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { repeated CommandSpec command = 50000; }
message CommandSpec { string name = 1; repeated FixedField fixed = 5; }
message FixedField { string field = 1; string value = 2; string note = 99; }
`,
				"test.proto": `syntax = "proto3";
package t.v1;
import "newer.proto";
service ThingService {
  rpc GetThing(Thing) returns (Thing) { option (hotbed.v1.command) = {name: "thing get", fixed: [{field: "id", value: "'a'", note: "future"}]}; }
}
message Thing { string id = 1; }
`,
			},
		},
		{
			name: "unknown field in ParamSpec",
			srcs: map[string]string{
				"newer.proto": `syntax = "proto3";
package hotbed.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.MethodOptions { repeated CommandSpec command = 50000; }
extend google.protobuf.FieldOptions { ParamSpec param = 50000; }
message CommandSpec { string name = 1; }
message ParamSpec { string name = 1; bool fancy = 99; }
`,
				"test.proto": `syntax = "proto3";
package t.v1;
import "newer.proto";
service ThingService {
  rpc GetThing(Thing) returns (Thing) { option (hotbed.v1.command) = {name: "thing get"}; }
}
message Thing { string id = 1 [(hotbed.v1.param) = {fancy: true}]; }
`,
			},
		},
		{
			name: "unknown field in GroupSpec",
			srcs: map[string]string{
				"newer.proto": `syntax = "proto3";
package hotbed.v1;
import "google/protobuf/descriptor.proto";
extend google.protobuf.ServiceOptions { repeated GroupSpec group = 50000; }
extend google.protobuf.MethodOptions { repeated CommandSpec command = 50000; }
message GroupSpec { string name = 1; string help = 2; bool hidden = 99; }
message CommandSpec { string name = 1; }
`,
				"test.proto": `syntax = "proto3";
package t.v1;
import "newer.proto";
service ThingService {
  option (hotbed.v1.group) = {name: "thing", help: "Things.", hidden: true};
  rpc GetThing(Thing) returns (Thing) { option (hotbed.v1.command) = {name: "thing get"}; }
}
message Thing { string id = 1; }
`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := descriptors(t, tc.srcs)
			if _, err := spec.Load(raw); err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "run hotbed upgrade") {
				t.Errorf("spec.Load = %v, want the unknown option refused", err)
			}
			if _, err := apicmd.Load(raw); err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "run hotbed upgrade") {
				t.Errorf("apicmd.Load = %v, want the unknown option refused", err)
			}
		})
	}
}

// Required flags (such as project rm-repo --repo) are validated by Kong before
// any RPC is called, advertised in MCP JSON Schema, and checked by Runner.Build.
func TestRequiredParamValidatedBeforeRPC(t *testing.T) {
	s, err := apicmd.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.Kong()
	if err != nil {
		t.Fatal(err)
	}
	parser, err := kong.New(&struct{}{}, k.Options()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse([]string{"project", "rm-repo", "myproj"}); err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("project rm-repo without --repo: err = %v, want missing --repo error", err)
	}
	if _, err := parser.Parse([]string{"project", "rm-repo", "myproj", "--repo", "github.com/o/r"}); err != nil {
		t.Errorf("project rm-repo with --repo: unexpected err = %v", err)
	}

	var rmTool *apicmd.Tool
	for i := range s.Tools() {
		tl := s.Tools()[i]
		if tl.Name == "project_rm-repo" {
			rmTool = &tl
			break
		}
	}
	if rmTool == nil {
		t.Fatal("missing MCP tool project_rm-repo")
	}
	reqFields, _ := rmTool.InputSchema["required"].([]string)
	hasRepo := false
	for _, r := range reqFields {
		if r == "repo" {
			hasRepo = true
		}
	}
	if !hasRepo {
		t.Errorf("project_rm-repo MCP schema required = %v, want repo included", reqFields)
	}

	rmCmd := s.Command("project rm-repo")
	r := &apicmd.Runner{Hooks: apicmd.NewHooks(), Sessions: noSessions{}}
	if _, _, err := r.Build(t.Context(), rmCmd, apicmd.Args{Given: map[*spec.Param]any{
		rmCmd.Params[0]: "myproj",
	}}); err == nil || !strings.Contains(err.Error(), "--repo is required") {
		t.Errorf("Runner.Build without repo = %v, want --repo is required", err)
	}
}

// A command's params are its request's fields in order, a flattened
// message's in its place; an enum's words drop its prefix and its zero.
func TestParams(t *testing.T) {
	srcs := api(`option (hotbed.v1.command) = {name: "thing get", session: true};`, `
  // The thing.
  string id = 1 [(hotbed.v1.param) = {positional: true}];
  Thing more = 2 [(hotbed.v1.param) = {flatten: true}];
  repeated Color colors = 3 [(hotbed.v1.param) = {name: "color", every: "all"}];
  string session_id = 4 [(hotbed.v1.param) = {hidden: true, default: "session"}];
  bool all_things = 5 [(hotbed.v1.param) = {local: true}];`)
	srcs["test.proto"] = strings.Replace(srcs["test.proto"], "message Thing { string id = 1; }", `message Thing { string thing_id = 1 [(hotbed.v1.param) = {short: "t"}]; }`, 1)
	s, err := apicmd.Load(descriptors(t, srcs))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Command("thing get")
	var got []string
	for _, p := range c.Params {
		got = append(got, p.Name)
	}
	if want := "id thing-id color session-id all-things"; strings.Join(got, " ") != want {
		t.Errorf("params = %v, want %s", got, want)
	}
	if p := c.Params[0]; !p.Positional || p.Help != "The thing." {
		t.Errorf("id = %+v", p)
	}
	if p := c.Params[1]; len(p.Path) != 2 || p.Short != "t" {
		t.Errorf("thing-id = %+v, want more.thing_id, -t", p)
	}
	if w := c.Params[2].Words(); len(w) != 1 || w[0].Word != "red" || w[0].Number != 1 {
		t.Errorf("color's words = %v, want [red]", w)
	}
	if p := c.Params[4]; !p.Local {
		t.Errorf("all-things = %+v, want local", p)
	}
}

// Two commands with the same params are still two: each runs as itself.
func TestAlikeCommandsRunAsThemselves(t *testing.T) {
	s, err := apicmd.Load(descriptors(t, api(`option (hotbed.v1.command) = {name: "thing get"};`, `string id = 1;`)))
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.Kong()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"thing get", "thing put"} {
		parser, err := kong.New(&struct{}{}, k.Options()...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := parser.Parse(append(strings.Fields(name), "--id", "x"))
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := k.Selected(ctx.Selected()); !ok || p.Command.Name() != name {
			t.Errorf("%s ran %s", name, p.Command.Name())
		}
	}
}

// building is a test API whose command exercises how a request is built.
const building = `syntax = "proto3";
package build.v1;
import "hotbed/v1/hotbed.proto";
service ThingService {
  rpc GetThing(GetThingRequest) returns (GetThingRequest) {
    option (hotbed.v1.command) = {name: "thing get", input: {}, fixed: [{field: "limit", value: "0"}]};
  }
}
message Place {
  oneof at {
    int32 before = 1;
    bool last = 2;
  }
}
message GetThingRequest {
  bool loud = 1 [(hotbed.v1.param) = {default: "true"}];
  string who = 2 [(hotbed.v1.param) = {hook: "Who"}];
  string mood = 3 [(hotbed.v1.param) = {default: "'calm'"}];
  Place place = 4 [(hotbed.v1.param) = {flatten: true}];
  int32 limit = 5;
  string secret = 6 [(hotbed.v1.param) = {hidden: true, default: "'kept'"}];
  oneof scope {
    string project = 7 [(hotbed.v1.param) = {default: "'home'"}];
    bool all_projects = 8;
  }
  bool dry = 9 [(hotbed.v1.param) = {local: true}];
  string plan = 10 [(hotbed.v1.param) = {hook: "Plan"}];
  string from_env = 11 [(hotbed.v1.param) = {default: "env:APICMD_TEST_ENV"}];
  optional int32 opt_limit = 12;
}
`

type noSessions struct{}

func (noSessions) Session(context.Context, string) (string, error) { return "", nil }
func (noSessions) Named(context.Context, string) (string, error)   { return "", nil }

// build builds thing get's request from the JSON and what was given.
func build(t *testing.T, input string, given map[string]any) (protoreflect.Message, error) {
	t.Helper()
	return buildWith(t, thingGet(t), input, given)
}

// thingGet is building's command.
func thingGet(t *testing.T) *apicmd.Command {
	t.Helper()
	s, err := apicmd.Load(descriptors(t, map[string]string{"test.proto": building}))
	if err != nil {
		t.Fatal(err)
	}
	return s.Command("thing get")
}

// buildWith builds c's request from the JSON and what was given.
func buildWith(t *testing.T, c *apicmd.Command, input string, given map[string]any) (protoreflect.Message, error) {
	t.Helper()
	// Hooks take their request as its Go type; this API's are dynamic.
	d := c.Method.Input()
	if _, err := protoregistry.GlobalTypes.FindMessageByName(d.FullName()); err != nil {
		if err := protoregistry.GlobalTypes.RegisterMessage(dynamicpb.NewMessageType(d)); err != nil {
			t.Fatal(err)
		}
	}
	hooks := apicmd.NewHooks()
	hooks.Fields["Who"] = func(_ context.Context, _ *apicmd.Call, req proto.Message) (any, error) {
		m := req.ProtoReflect()
		return "saw " + m.Get(m.Descriptor().Fields().ByName("mood")).String(), nil
	}
	hooks.Fields["Plan"] = func(_ context.Context, _ *apicmd.Call, req proto.Message) (any, error) {
		m := req.ProtoReflect()
		if m.Get(m.Descriptor().Fields().ByName("dry")).Bool() {
			return "dry", nil
		}
		return "wet", nil
	}
	a := apicmd.Args{Given: map[*spec.Param]any{}, Input: []byte(input)}
	for _, p := range c.Params {
		if v, ok := given[p.Name]; ok {
			a.Given[p] = v
		}
	}
	r := &apicmd.Runner{Hooks: hooks, Sessions: noSessions{}}
	req, _, err := r.Build(t.Context(), c, a)
	if err != nil {
		return nil, err
	}
	return req, nil
}

func field(m protoreflect.Message, name string) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(name)))
}

// What is given, even a zero (--loud=false), is not defaulted over; a
// hook sees every CEL default, whatever the fields' order.
func TestGivenZeroIsGivenAndHooksSeeDefaults(t *testing.T) {
	req, err := build(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !field(req, "loud").Bool() || field(req, "who").String() != "saw calm" {
		t.Errorf("built %v, want loud by default, and the hook seeing mood's default", req)
	}
	req, err = build(t, `{}`, map[string]any{"loud": false, "mood": "grim"})
	if err != nil {
		t.Fatal(err)
	}
	if field(req, "loud").Bool() || field(req, "who").String() != "saw grim" {
		t.Errorf("built %v, want --loud=false kept and the hook seeing the mood given", req)
	}
}

// A zero given for a flattened message's field makes no message, and
// picks no member of its oneof.
func TestZeroMakesNoMessage(t *testing.T) {
	req, err := build(t, `{}`, map[string]any{"before": int32(0)})
	if err != nil {
		t.Fatal(err)
	}
	if req.Has(req.Descriptor().Fields().ByName("place")) {
		t.Errorf("built %v, want no place", req)
	}
	req, err = build(t, `{}`, map[string]any{"last": true})
	if err != nil {
		t.Fatal(err)
	}
	if place := field(req, "place").Message(); !field(place, "last").Bool() {
		t.Errorf("built %v, want place last", req)
	}
}

// The JSON may not set what the command fixes, nor what hotbed fills in.
func TestJSONSetsNoFixedOrHiddenField(t *testing.T) {
	if _, err := build(t, `{"limit": 5}`, nil); err == nil || !strings.Contains(err.Error(), "the JSON sets limit, which thing get fixes") {
		t.Errorf("build = %v, want the fixed field refused", err)
	}
	if _, err := build(t, `{"secret": "mine"}`, nil); err == nil || !strings.Contains(err.Error(), "the JSON sets secret, which hotbed fills in") {
		t.Errorf("build = %v, want the hidden field refused", err)
	}
	req, err := build(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if field(req, "secret").String() != "kept" {
		t.Errorf("built %v, want the hidden field's default", req)
	}
}

// A oneof's member takes no default while another member has a value; two
// members given is an error, not the last one winning.
func TestOneofMemberYieldsToAnother(t *testing.T) {
	req, err := build(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := field(req, "project").String(); got != "home" {
		t.Errorf("project = %q, want its default", got)
	}
	req, err = build(t, `{}`, map[string]any{"all-projects": true})
	if err != nil {
		t.Fatal(err)
	}
	if w := req.WhichOneof(req.Descriptor().Oneofs().ByName("scope")); w == nil || w.Name() != "all_projects" {
		t.Errorf("built %v, want all_projects, and project's default yielding to it", req)
	}
	if _, err := build(t, `{}`, map[string]any{"project": "away", "all-projects": true}); err == nil || !strings.Contains(err.Error(), "all_projects and project are one or the other") {
		t.Errorf("build = %v, want two members of scope refused", err)
	}
	if _, err := build(t, `{"project": "away"}`, map[string]any{"all-projects": true}); err == nil || !strings.Contains(err.Error(), "all_projects and project are one or the other") {
		t.Errorf("build = %v, want a member given over the JSON's refused", err)
	}
}

// A local field is the command's to read, hooks' too, and never sent.
func TestLocalFieldIsReadAndNotSent(t *testing.T) {
	c := thingGet(t)
	req, err := buildWith(t, c, `{}`, map[string]any{"dry": true})
	if err != nil {
		t.Fatal(err)
	}
	if !field(req, "dry").Bool() || field(req, "plan").String() != "dry" {
		t.Errorf("built %v, want dry kept and plan's hook reading it", req)
	}
	wire := apicmd.Wire(c, req.(*dynamicpb.Message))
	if wire.Has(wire.Descriptor().Fields().ByName("dry")) || field(wire, "plan").String() != "dry" {
		t.Errorf("sent %v, want dry cleared and the rest as built", wire)
	}
	if !field(req, "dry").Bool() {
		t.Errorf("Wire cleared dry in the built request %v too; hooks after the call read it", req)
	}
}

// env:VAR defaults read the process environment; proto3 optional scalars keep
// an explicitly given zero value (presence = true) while remaining unset when
// omitted.
func TestEnvDefaultAndOptionalScalarZero(t *testing.T) {
	t.Setenv("APICMD_TEST_ENV", "hello-env")
	req, err := build(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := field(req, "from_env").String(); got != "hello-env" {
		t.Errorf("from_env = %q, want hello-env", got)
	}
	if req.Has(req.Descriptor().Fields().ByName("opt_limit")) {
		t.Errorf("opt_limit should be unset when omitted")
	}

	req, err = build(t, `{}`, map[string]any{"opt-limit": int32(0)})
	if err != nil {
		t.Fatal(err)
	}
	f := req.Descriptor().Fields().ByName("opt_limit")
	if !req.Has(f) || req.Get(f).Int() != 0 {
		t.Errorf("opt_limit given 0: Has=%v Val=%v, want true/0", req.Has(f), req.Get(f))
	}
}

func TestRunnerPassesIdempotencyLevelToConnectInterceptor(t *testing.T) {
	s, err := apicmd.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	wantLevel := func(m protoreflect.MethodDescriptor) connect.IdempotencyLevel {
		opts, _ := m.Options().(*descriptorpb.MethodOptions)
		switch opts.GetIdempotencyLevel() {
		case descriptorpb.MethodOptions_NO_SIDE_EFFECTS:
			return connect.IdempotencyNoSideEffects
		case descriptorpb.MethodOptions_IDEMPOTENT:
			return connect.IdempotencyIdempotent
		default:
			return connect.IdempotencyUnknown
		}
	}
	for _, c := range s.Commands {
		got := apicmd.Spec(c).IdempotencyLevel
		want := wantLevel(c.Method)
		if got != want {
			t.Errorf("%s: Spec().IdempotencyLevel = %v, want %v (from descriptor)", c.Name(), got, want)
		}
		// Cross-check against compiled hotbed.proto file descriptor too.
		svc := pb.File_hotbed_v1_hotbed_proto.Services().ByName(c.Method.Parent().Name())
		if svc == nil {
			t.Fatalf("%s: service %s not in compiled hotbed.proto", c.Name(), c.Method.Parent().Name())
		}
		cm := svc.Methods().ByName(c.Method.Name())
		if cm == nil {
			t.Fatalf("%s: method %s not in compiled hotbed.proto", c.Name(), c.Method.Name())
		}
		if got != wantLevel(cm) {
			t.Errorf("%s: Spec().IdempotencyLevel = %v, compiled proto has %v", c.Name(), got, wantLevel(cm))
		}
	}
	// Spot-check known idempotent and non-idempotent commands.
	for _, name := range []string{"question ls", "question outbox", "question show", "poll", "question projects"} {
		c := s.Command(name)
		if c == nil {
			t.Fatalf("missing command %q", name)
		}
		if got := apicmd.Spec(c).IdempotencyLevel; got != connect.IdempotencyIdempotent {
			t.Errorf("%s: IdempotencyLevel = %v, want IdempotencyIdempotent", name, got)
		}
	}
	for _, name := range []string{"question add", "question edit", "question rm", "question ack"} {
		c := s.Command(name)
		if c == nil {
			t.Fatalf("missing command %q", name)
		}
		if got := apicmd.Spec(c).IdempotencyLevel; got != connect.IdempotencyUnknown {
			t.Errorf("%s: IdempotencyLevel = %v, want IdempotencyUnknown", name, got)
		}
	}

	// Verify all three protobuf IdempotencyLevel values flow through Runner.Run into a Connect interceptor.
	raw := descriptors(t, map[string]string{"test.proto": `syntax = "proto3";
package t.v1;
import "hotbed/v1/hotbed.proto";
service IdemService {
  rpc ReadOnly(Empty) returns (Empty) {
    option idempotency_level = NO_SIDE_EFFECTS;
    option (hotbed.v1.command) = {name: "idem read"};
  }
  rpc Idem(Empty) returns (Empty) {
    option idempotency_level = IDEMPOTENT;
    option (hotbed.v1.command) = {name: "idem put"};
  }
  rpc Mutate(Empty) returns (Empty) {
    option (hotbed.v1.command) = {name: "idem add"};
  }
}
message Empty {}
`})
	ts, err := apicmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cmd  string
		want connect.IdempotencyLevel
	}{
		{"idem read", connect.IdempotencyNoSideEffects},
		{"idem put", connect.IdempotencyIdempotent},
		{"idem add", connect.IdempotencyUnknown},
	} {
		var seen connect.IdempotencyLevel
		var called bool
		stopErr := connect.NewError(connect.CodeAborted, "stop")
		interceptor := connect.ClientInterceptor(func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, sp connect.Spec) (connect.ClientStream, error) {
				called = true
				seen = sp.IdempotencyLevel
				return nil, stopErr
			}
		})
		client, err := hotapi.Dial("127.0.0.1:1", hotapi.DefaultPort, http.DefaultClient, interceptor)
		if err != nil {
			t.Fatal(err)
		}
		r := &apicmd.Runner{Calls: client, Waits: client}
		_, _ = r.Run(t.Context(), ts.Command(tc.cmd), apicmd.Args{})
		if !called || seen != tc.want {
			t.Errorf("%s: interceptor saw called=%v IdempotencyLevel=%v, want true/%v", tc.cmd, called, seen, tc.want)
		}
	}
}

// RenderCommandReference skips message params just like Kong does, so the
// generated skill reference never documents nonexistent CLI flags like
// --questions or --prompt (board 7894).
func TestRenderCommandReferenceSkipsMessageParams(t *testing.T) {
	s, err := apicmd.Load(hotbedv1cli.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	doc := apicmd.RenderCommandReference(s.Schema)
	for _, bad := range []string{"`--questions`", "`--prompt`"} {
		if strings.Contains(doc, bad) {
			t.Errorf("RenderCommandReference documented nonexistent message flag %s", bad)
		}
	}
	// Class check: no message param on any command is rendered as a flag in its
	// own command section, and every documented --flag matches a real Kong flag.
	flagRE := regexp.MustCompile("`--([a-z0-9-]+)`")
	k, err := s.Kong()
	if err != nil {
		t.Fatal(err)
	}
	parser, err := kong.New(&struct{}{}, k.Options()...)
	if err != nil {
		t.Fatal(err)
	}
	realFlags := map[string]map[string]bool{}
	var walk func(n *kong.Node, prefix []string)
	walk = func(n *kong.Node, prefix []string) {
		if n.Type == kong.CommandNode {
			prefix = append(append([]string(nil), prefix...), n.Name)
			if len(n.Children) == 0 {
				set := map[string]bool{}
				for _, f := range n.Flags {
					set[f.Name] = true
				}
				realFlags[strings.Join(prefix, " ")] = set
			}
		}
		for _, ch := range n.Children {
			walk(ch, prefix)
		}
	}
	walk(parser.Model.Node, nil)

	docFlags := map[string]map[string]bool{}
	var currentCmd string
	for _, line := range strings.Split(doc, "\n") {
		if rest, ok := strings.CutPrefix(line, "### `hotbed "); ok {
			sig, _, _ := strings.Cut(rest, "`")
			var words []string
			for _, w := range strings.Fields(sig) {
				if w == strings.ToUpper(w) {
					break // positional arg
				}
				words = append(words, w)
			}
			currentCmd = strings.Join(words, " ")
			continue
		}
		if currentCmd != "" && strings.HasPrefix(line, "| ") {
			col := strings.Split(line, "|")[1]
			for _, m := range flagRE.FindAllStringSubmatch(col, -1) {
				if docFlags[currentCmd] == nil {
					docFlags[currentCmd] = map[string]bool{}
				}
				docFlags[currentCmd][m[1]] = true
				if !realFlags[currentCmd][m[1]] {
					t.Errorf("%s: skilldoc documents --%s, which Kong does not accept", currentCmd, m[1])
				}
			}
		}
	}
	for _, c := range s.Commands {
		for _, p := range c.Params {
			if p.Message() && docFlags[c.Name()][p.Name] {
				t.Errorf("%s: message param --%s was rendered in skilldoc", c.Name(), p.Name)
			}
		}
	}
}

func TestOptionalPositionalParam(t *testing.T) {
	s, err := apicmd.Load(descriptors(t, api(
		`option (hotbed.v1.command) = {name: "thing get"};`,
		`string id = 1 [(hotbed.v1.param) = {positional: true, optional: true}];`,
	)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	k, err := s.Kong()
	if err != nil {
		t.Fatalf("Kong: %v", err)
	}
	parser, err := kong.New(&struct{}{}, k.Options()...)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	// Parsing without the optional positional arg succeeds.
	kctx, err := parser.Parse([]string{"thing", "get"})
	if err != nil {
		t.Fatalf("Parse without optional arg: %v", err)
	}
	parsed, ok := k.Selected(kctx.Selected())
	if !ok || len(parsed.Args.Given) != 0 {
		t.Fatalf("Selected without arg = (%+v, %v), want empty Given", parsed, ok)
	}
	// Parsing with the positional arg captures it.
	kctx, err = parser.Parse([]string{"thing", "get", "abc"})
	if err != nil {
		t.Fatalf("Parse with optional arg: %v", err)
	}
	parsed, ok = k.Selected(kctx.Selected())
	if !ok || len(parsed.Args.Given) != 1 {
		t.Fatalf("Selected with arg = (%+v, %v), want 1 Given", parsed, ok)
	}
	if got := parsed.Args.Given[s.Command("thing get").Params[0]]; got != "abc" {
		t.Fatalf("Selected arg value = %v, want %q", got, "abc")
	}
	// MCP input schema marks it not required.
	foundTool := false
	for _, tool := range s.Tools() {
		if tool.Name == "thing_get" {
			foundTool = true
			if req, ok := tool.InputSchema["required"]; ok {
				t.Fatalf("thing_get MCP schema has required=%v, want none", req)
			}
		}
	}
	if !foundTool {
		t.Fatal("MCP tool thing_get not found")
	}
	// Skilldoc renders optional positional as [ID] and repeated positional as [ITEMS...].
	if doc := apicmd.RenderCommandReference(s.Schema); !strings.Contains(doc, "### `hotbed thing get [ID]`") {
		t.Errorf("skilldoc missing [ID]:\n%s", doc)
	}
	rep, err := apicmd.Load(descriptors(t, api(
		`option (hotbed.v1.command) = {name: "thing get"};`,
		`string name = 1 [(hotbed.v1.param) = {positional: true}]; repeated string items = 2 [(hotbed.v1.param) = {positional: true}];`,
	)))
	if err != nil {
		t.Fatalf("Load repeated positional: %v", err)
	}
	if doc := apicmd.RenderCommandReference(rep.Schema); !strings.Contains(doc, "### `hotbed thing get NAME [ITEMS...]`") {
		t.Errorf("skilldoc missing [ITEMS...]:\n%s", doc)
	}
	// Invalid combinations rejected by Load.
	for _, tc := range []struct {
		name   string
		fields string
		want   string
	}{
		{
			name:   "optional on flag",
			fields: `string id = 1 [(hotbed.v1.param) = {optional: true}];`,
			want:   "optional is for a positional argument",
		},
		{
			name:   "optional and required",
			fields: `string id = 1 [(hotbed.v1.param) = {positional: true, optional: true, required: true}];`,
			want:   "both optional and required",
		},
		{
			name:   "required positional after optional positional",
			fields: `string a = 1 [(hotbed.v1.param) = {positional: true, optional: true}]; string b = 2 [(hotbed.v1.param) = {positional: true}];`,
			want:   "required positional argument cannot follow optional positional",
		},
		{
			name:   "required repeated positional after optional positional",
			fields: `string a = 1 [(hotbed.v1.param) = {positional: true, optional: true}]; repeated string b = 2 [(hotbed.v1.param) = {positional: true, required: true}];`,
			want:   "required positional argument cannot follow optional positional",
		},
		{
			name:   "required positional after repeated positional",
			fields: `repeated string a = 1 [(hotbed.v1.param) = {positional: true}]; string b = 2 [(hotbed.v1.param) = {positional: true}];`,
			want:   "positional argument cannot follow repeated positional",
		},
		{
			name:   "optional positional after repeated positional",
			fields: `repeated string a = 1 [(hotbed.v1.param) = {positional: true}]; string b = 2 [(hotbed.v1.param) = {positional: true, optional: true}];`,
			want:   "positional argument cannot follow repeated positional",
		},
		{
			name:   "repeated positional after repeated positional",
			fields: `repeated string a = 1 [(hotbed.v1.param) = {positional: true}]; repeated string b = 2 [(hotbed.v1.param) = {positional: true}];`,
			want:   "positional argument cannot follow repeated positional",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := apicmd.Load(descriptors(t, api(
				`option (hotbed.v1.command) = {name: "thing get"};`,
				tc.fields,
			))); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
