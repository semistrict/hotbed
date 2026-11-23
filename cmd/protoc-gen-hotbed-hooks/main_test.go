package main

import (
	"os"
	"path/filepath"
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
