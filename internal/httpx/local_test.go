package httpx

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/semistrict/hotbed/internal/api"
)

// thingRequest is a request with a local field, dry, compiled at run time
// as a newer API's would be.
func thingRequest(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	const src = `syntax = "proto3";
package t.v1;
import "hotbed/v1/hotbed.proto";
message PutThingRequest {
  string id = 1;
  bool dry = 2 [(hotbed.v1.param) = {local: true}];
}
`
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: []string{"../../proto", "."},
			Accessor: func(path string) (io.ReadCloser, error) {
				if path == "test.proto" {
					return io.NopCloser(strings.NewReader(src)), nil
				}
				return os.Open(path)
			},
		}),
	}
	files, err := c.Compile(t.Context(), "test.proto")
	if err != nil {
		t.Fatal(err)
	}
	return files[0].Messages().ByName("PutThingRequest")
}

// stream receives one request.
type stream struct {
	connect.ServerStream
	req proto.Message
}

func (s stream) Receive(msg any) error {
	proto.Merge(msg.(proto.Message), s.req)
	return nil
}

// The server refuses a request that sets a local field, the client's
// alone, rather than ignore it; one that leaves it unset reaches its
// handler.
func TestServerRefusesLocalFields(t *testing.T) {
	md := thingRequest(t)
	server := NewServer()
	reached := 0
	server.Register(connect.Method{
		Spec: connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/t.v1.ThingService/PutThing"},
		Handler: func(ctx context.Context, _ connect.Spec, s connect.ServerStream) error {
			if err := s.Receive(dynamicpb.NewMessage(md)); err != nil {
				return err
			}
			reached++
			return nil
		},
	})
	call := func(req proto.Message) error {
		ctx := WithPeer(t.Context(), Peer{})
		return server.Call(ctx, "/t.v1.ThingService/PutThing", nil, stream{req: req})
	}
	req := dynamicpb.NewMessage(md)
	req.Set(md.Fields().ByName("id"), protoreflect.ValueOfString("a"))
	if err := call(req); err != nil || reached != 1 {
		t.Errorf("call with dry unset = %v, reached %d, want it handled", err, reached)
	}
	req.Set(md.Fields().ByName("dry"), protoreflect.ValueOfBool(true))
	err := call(req)
	if e := api.FromConnect(err); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "t.v1.PutThingRequest.dry is the client's alone (local)") || reached != 1 {
		t.Errorf("call with dry set = %v, reached %d, want INVALID_ARGS before the handler acts", err, reached)
	}
}
