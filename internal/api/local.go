package api

import (
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// RefuseLocalFields fails, INVALID_ARGS, a request that sets a local field
// (hotbed.v1.param's local): one the client's commands read and clear
// before the call, which the server never acts on.
func RefuseLocalFields(m protoreflect.Message) error {
	fs, err := localFields(m.Descriptor())
	if err != nil {
		return err
	}
	for _, f := range fs {
		if m.Has(f) {
			return Errorf(CodeInvalidArgs, "%s is the client's alone (local): no request to the server sets it", f.FullName()).
				WithSuggestion("leave it unset; only hotbed's commands read it")
		}
	}
	return nil
}

// locals caches each request's local fields by its full name.
var locals sync.Map // protoreflect.FullName -> []protoreflect.FieldDescriptor

func localFields(md protoreflect.MessageDescriptor) ([]protoreflect.FieldDescriptor, error) {
	if v, ok := locals.Load(md.FullName()); ok {
		return v.([]protoreflect.FieldDescriptor), nil
	}
	var out []protoreflect.FieldDescriptor
	for i := range md.Fields().Len() {
		f := md.Fields().Get(i)
		local, err := isLocal(f)
		if err != nil {
			return nil, err
		}
		if local {
			out = append(out, f)
		}
	}
	locals.Store(md.FullName(), out)
	return out, nil
}

// isLocal reads f's param option however its descriptor was made: decoded
// again against this binary's types, a descriptor built at run time reads
// as one compiled in.
func isLocal(f protoreflect.FieldDescriptor) (bool, error) {
	opts, ok := f.Options().(*descriptorpb.FieldOptions)
	if !ok {
		return false, fmt.Errorf("read %s's options: a %T, not field options", f.FullName(), f.Options())
	}
	if opts == nil {
		return false, nil
	}
	b, err := proto.Marshal(opts)
	if err != nil {
		return false, fmt.Errorf("read %s's options: %w", f.FullName(), err)
	}
	decoded := &descriptorpb.FieldOptions{}
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(b, decoded); err != nil {
		return false, fmt.Errorf("read %s's options: %w", f.FullName(), err)
	}
	return proto.GetExtension(decoded, pb.E_Param).(*pb.ParamSpec).GetLocal(), nil
}
