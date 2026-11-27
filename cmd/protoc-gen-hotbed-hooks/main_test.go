package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// request is what buf gives the plugin for hotbed.proto.
func request(t *testing.T) *pluginpb.CodeGeneratorRequest {
	t.Helper()
	c := protocompile.Compiler{
		Resolver:       protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{"../../proto"}}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	const file = "hotbed/v1/hotbed.proto"
	files, err := c.Compile(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	req := &pluginpb.CodeGeneratorRequest{FileToGenerate: []string{file}, Parameter: proto.String("paths=source_relative")}
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
		req.ProtoFile = append(req.ProtoFile, protodesc.ToFileDescriptorProto(f))
	}
	add(files[0])
	// Read again, as the plugin reads its stdin: the options' extensions
	// this binary knows are then its own types.
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out := &pluginpb.CodeGeneratorRequest{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// generated is what the plugin makes of req, by file name.
func generated(t *testing.T, req *pluginpb.CodeGeneratorRequest) map[string]string {
	t.Helper()
	gen, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(gen); err != nil {
		t.Fatal(err)
	}
	res := gen.Response()
	if res.Error != nil {
		t.Fatal(res.GetError())
	}
	out := map[string]string{}
	for _, f := range res.File {
		out[f.GetName()] = f.GetContent()
	}
	return out
}

// The plugin makes the same files each run, and the ones checked in are
// current: the Go byte for byte; the descriptors, which buf builds with
// its own source spans and well-known types, as hotbed.proto's descriptor
// apart from source info.
func TestGeneratedFilesAreStableAndCurrent(t *testing.T) {
	req := request(t)
	first := generated(t, req)
	if len(first) == 0 {
		t.Fatal("the plugin made no files")
	}
	for range 5 {
		for name, content := range generated(t, req) {
			if content != first[name] {
				t.Fatalf("%s differs from one run to the next", name)
			}
		}
	}
	for name, content := range first {
		checked, err := os.ReadFile(filepath.Join("../../internal/gen", name))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(name) == ".binpb" {
			if !proto.Equal(hotbedFile(t, checked), hotbedFile(t, []byte(content))) {
				t.Errorf("internal/gen/%s is stale: run go tool buf generate", name)
			}
		} else if string(checked) != content {
			t.Errorf("internal/gen/%s is stale: run go tool buf generate", name)
		}
	}
}

// hotbedFile is hotbed.proto's descriptor in a descriptor set, without
// source info.
func hotbedFile(t *testing.T, set []byte) *descriptorpb.FileDescriptorProto {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(set, fds); err != nil {
		t.Fatal(err)
	}
	for _, f := range fds.File {
		if f.GetName() == "hotbed/v1/hotbed.proto" {
			f.SourceCodeInfo = nil
			return f
		}
	}
	t.Fatal("no hotbed/v1/hotbed.proto in the descriptors")
	return nil
}

// A package's services may be in several files: the plugin makes one Go
// package of their hooks, every file's in its one Hooks interface.
func TestServicesAcrossFilesOfAPackage(t *testing.T) {
	srcs := map[string]string{
		"split/v1/a.proto": `syntax = "proto3";
package split.v1;
import "hotbed/v1/hotbed.proto";
option go_package = "example.com/split/v1;splitv1";
service AService {
  rpc A(ARequest) returns (ARequest) { option (hotbed.v1.command) = {name: "split a", before: ["BeforeA"]}; }
}
message ARequest { string id = 1 [(hotbed.v1.param) = {hook: "Shared"}]; }
`,
		"split/v1/b.proto": `syntax = "proto3";
package split.v1;
import "hotbed/v1/hotbed.proto";
import "split/v1/a.proto";
option go_package = "example.com/split/v1;splitv1";
service BService {
  rpc B(BRequest) returns (ARequest) { option (hotbed.v1.command) = {name: "split b"}; }
}
message BRequest { string id = 1 [(hotbed.v1.param) = {hook: "Shared"}]; }
`,
	}
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
	files, err := c.Compile(t.Context(), "split/v1/a.proto", "split/v1/b.proto")
	if err != nil {
		t.Fatal(err)
	}
	req := &pluginpb.CodeGeneratorRequest{FileToGenerate: []string{"split/v1/a.proto", "split/v1/b.proto"}, Parameter: proto.String("paths=source_relative")}
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
		req.ProtoFile = append(req.ProtoFile, protodesc.ToFileDescriptorProto(f))
	}
	for _, f := range files {
		add(f)
	}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	in := &pluginpb.CodeGeneratorRequest{}
	if err := proto.Unmarshal(b, in); err != nil {
		t.Fatal(err)
	}
	out := generated(t, in)
	src, ok := out["split/v1/splitv1cli/splitv1cli.go"]
	if !ok {
		t.Fatalf("no split/v1/splitv1cli/splitv1cli.go among %v", slices.Sorted(maps.Keys(out)))
	}
	if _, ok := out["split/v1/splitv1cli/descriptors.binpb"]; !ok {
		t.Errorf("no descriptors among %v", slices.Sorted(maps.Keys(out)))
	}
	for _, want := range []string{
		"BeforeA(ctx context.Context, c *apicmd.Call, req *v1.ARequest) error",
		// Named for two requests, in two files, it sees neither.
		"Shared(ctx context.Context, c *apicmd.Call) (string, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated hooks lack %q:\n%s", want, src)
		}
	}
	// hotbed.v1's own hooks are its package's, not this one's.
	if strings.Contains(src, "PollCursor") {
		t.Errorf("split.v1's hooks include hotbed.v1's:\n%s", src)
	}
}
