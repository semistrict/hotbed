package apicmd

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// Runner runs commands against an API.
type Runner struct {
	Hooks Hooks
	// Calls calls the API; Waits, for commands that wait, has no deadline.
	Calls, Waits *connect.Client
	Sessions     Sessions
}

// Args is what a command was given, by the CLI or an MCP client.
type Args struct {
	// Given is each param given, as a Go value of its type; an enum's as
	// its word, or words.
	Given map[*spec.Param]any
	// Session is -s's value: "" when not given.
	Session string
	// Input is the command's JSON; nil when none was given.
	Input []byte
}

// Run builds c's request from a and calls its RPC, with the command's
// before hooks first and after hooks last.
func (r *Runner) Run(ctx context.Context, c *Command, a Args) (proto.Message, error) {
	req, call, err := r.Build(ctx, c, a)
	if err != nil {
		return nil, err
	}
	for _, name := range c.Before {
		t, err := typed(req)
		if err != nil {
			return nil, err
		}
		if err := r.Hooks.Before[name](ctx, call, t); err != nil {
			return nil, err
		}
		if err := assign(req, t); err != nil {
			return nil, err
		}
	}
	client := r.Calls
	if c.Waits {
		client = r.Waits
	}
	res := dynamicpb.NewMessage(c.Method.Output())
	if err := client.CallUnary(ctx, Spec(c), Wire(c, req), res); err != nil {
		return nil, err
	}
	if len(c.After) == 0 {
		return res, nil
	}
	treq, err := typed(req)
	if err != nil {
		return nil, err
	}
	tres, err := typed(res)
	if err != nil {
		return nil, err
	}
	for _, name := range c.After {
		if err := r.Hooks.After[name](ctx, call, treq, tres); err != nil {
			return nil, err
		}
	}
	return tres, nil
}

// Spec is how c's call is made: its procedure, and whether it is
// idempotent, which lets a client make it again when its peer restarts
// under it (api.RideOut).
func Spec(c *Command) connect.Spec {
	m := c.Method
	return connect.Spec{
		StreamType:       connect.StreamTypeUnary,
		Schema:           m,
		Procedure:        fmt.Sprintf("/%s/%s", m.Parent().FullName(), m.Name()),
		IdempotencyLevel: idempotencyLevel(m),
	}
}

func idempotencyLevel(m protoreflect.MethodDescriptor) connect.IdempotencyLevel {
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

// Wire is the request as sent: without its local fields, the client's
// alone.
func Wire(c *Command, req *dynamicpb.Message) *dynamicpb.Message {
	out := req
	for _, p := range c.Params {
		if p.Local && req.Has(p.Field()) {
			if out == req {
				out = proto.Clone(req).(*dynamicpb.Message)
			}
			out.Clear(p.Field())
		}
	}
	return out
}

// Build builds c's request from a: its fields take its JSON and what was
// given, then the command's fixed values, then (what is still not given,
// nor taken by another member of its oneof) their defaults, then their
// hooks. Two members of a oneof given is an error.
func (r *Runner) Build(ctx context.Context, c *Command, a Args) (*dynamicpb.Message, *Call, error) {
	if missing := r.missingHooks(c); len(missing) > 0 {
		return nil, nil, api.Errorf(api.CodeInvalidArgs, "%s needs hooks this hotbed does not have: %s", c.Name(), strings.Join(missing, ", ")).
			WithSuggestion("run hotbed upgrade: the controller's API is newer than this client")
	}
	call := &Call{Command: c, given: a.Session, sessions: r.Sessions}
	req := dynamicpb.NewMessage(c.Method.Input())

	if c.Input != nil {
		if err := r.input(ctx, call, req, a.Input); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range c.Params {
		v, ok := a.Given[p]
		if !ok {
			continue
		}
		if w := otherMember(req, p); w != nil && !zero(v) {
			return nil, nil, api.Errorf(api.CodeInvalidArgs, "%s and %s are one or the other: give one", p.Var(), c.paramOf(w)).WithSuggestion(helpOf(c))
		}
		if err := setParam(req, p, v, isOptionalScalar(p.Field())); err != nil {
			return nil, nil, api.Errorf(api.CodeInvalidArgs, "--%s: %v", p.Name, err)
		}
	}
	for i, fx := range c.Fixed {
		v, err := call.eval(ctx, c.fixed[i])
		if err != nil {
			return nil, nil, err
		}
		req.Clear(fx.Field)
		if err := setField(req, fx.Field, v, isOptionalScalar(fx.Field)); err != nil {
			return nil, nil, fmt.Errorf("%s's fixed %s: %w", c.Name(), fx.Field.Name(), err)
		}
	}
	// What was not given takes its default, then its hook's value: a hook
	// sees every default.
	unset := func(p *spec.Param) bool {
		_, given := a.Given[p]
		return !given && !isSet(req, p) && otherMember(req, p) == nil
	}
	for _, p := range c.Params {
		if p.Default == "" || !unset(p) {
			continue
		}
		v, err := call.eval(ctx, c.defaults[p])
		if err != nil {
			return nil, nil, err
		}
		if err := setParam(req, p, v, false); err != nil {
			return nil, nil, fmt.Errorf("%s's default for %s: %w", c.Name(), p.Field().FullName(), err)
		}
	}
	for _, p := range c.Params {
		if p.Hook == "" || !unset(p) {
			continue
		}
		v, err := r.fieldHook(ctx, call, p, req)
		if err != nil {
			return nil, nil, err
		}
		if err := setParam(req, p, v, false); err != nil {
			return nil, nil, fmt.Errorf("%s's hook %s: %w", c.Name(), p.Hook, err)
		}
	}
	for _, p := range c.Params {
		if !p.Required {
			continue
		}
		_, given := a.Given[p]
		if !given || (p.Field().IsList() && !isSet(req, p)) {
			return nil, nil, api.Errorf(api.CodeInvalidArgs, "--%s is required", p.Name).WithSuggestion(helpOf(c))
		}
	}
	return req, call, nil
}

func (r *Runner) missingHooks(c *Command) []string {
	var out []string
	note := func(name string, ok bool) {
		if !ok {
			out = append(out, name)
		}
	}
	for _, p := range c.Params {
		if p.Hook != "" {
			_, ok := r.Hooks.Fields[p.Hook]
			note(p.Hook, ok)
		}
	}
	if c.Input != nil && c.Input.Hook != "" {
		_, ok := r.Hooks.Inputs[c.Input.Hook]
		note(c.Input.Hook, ok)
	}
	for _, n := range c.Before {
		_, ok := r.Hooks.Before[n]
		note(n, ok)
	}
	for _, n := range c.After {
		_, ok := r.Hooks.After[n]
		note(n, ok)
	}
	return out
}

// input reads the command's JSON into the request, or the field it is.
func (r *Runner) input(ctx context.Context, call *Call, req *dynamicpb.Message, input []byte) error {
	c := call.Command
	if input == nil {
		return api.Errorf(api.CodeInvalidArgs, "%s takes JSON, and none was given", c.Name())
	}
	var target protoreflect.Message = req
	if f := c.Input.Field; f != nil {
		target = req.Mutable(f).Message()
	}
	if c.Input.Hook != "" {
		m, err := r.Hooks.Inputs[c.Input.Hook](ctx, call, input)
		if err != nil {
			return err
		}
		if err := assign(target, m); err != nil {
			return err
		}
	} else if err := protojson.Unmarshal(input, target.Interface()); err != nil {
		return api.Errorf(api.CodeInvalidArgs, "read the JSON: %v", err).WithSuggestion(helpOf(c))
	}
	// What hotbed fills in, or the command fixes, the JSON may not.
	for _, p := range c.Params {
		if p.Hidden && isSet(req, p) {
			return api.Errorf(api.CodeInvalidArgs, "the JSON sets %s, which hotbed fills in", p.Field().JSONName()).WithSuggestion(helpOf(c))
		}
	}
	for _, fx := range c.Fixed {
		if req.Has(fx.Field) {
			return api.Errorf(api.CodeInvalidArgs, "the JSON sets %s, which %s fixes", fx.Field.JSONName(), c.Name()).WithSuggestion(helpOf(c))
		}
	}
	return nil
}

func (r *Runner) fieldHook(ctx context.Context, call *Call, p *spec.Param, req *dynamicpb.Message) (any, error) {
	t, err := typed(req)
	if err != nil {
		return nil, err
	}
	return r.Hooks.Fields[p.Hook](ctx, call, t)
}

// typed is m as its Go type, which hooks take.
func typed(m protoreflect.Message) (proto.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(m.Descriptor().FullName())
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "this hotbed has no Go type for %s", m.Descriptor().FullName()).
			WithSuggestion("run hotbed upgrade: the controller's API is newer than this client")
	}
	out := mt.New().Interface()
	return out, assign(out.ProtoReflect(), m.Interface())
}

// assign makes dst a copy of src, which may be of another Go type for the
// same message.
func assign(dst protoreflect.Message, src proto.Message) error {
	b, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	proto.Reset(dst.Interface())
	return proto.Unmarshal(b, dst.Interface())
}

// paramOf names field f as it is given: its param, or its JSON.
func (c *Command) paramOf(f protoreflect.FieldDescriptor) string {
	for _, p := range c.Params {
		if p.Field() == f && !p.Hidden && !p.Message() {
			return p.Var()
		}
	}
	return "the JSON's " + f.JSONName()
}

// zero says whether v is no value: nil, or its type's zero.
func zero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

// helpOf is where a command's usage is.
func helpOf(c *Command) string { return "hotbed " + c.Name() + " --help" }

// isOptionalScalar says whether f is a proto3 optional scalar field (whose
// explicit zero value carries presence, such as --limit 0).
func isOptionalScalar(f protoreflect.FieldDescriptor) bool {
	o := f.ContainingOneof()
	return o != nil && o.IsSynthetic() && f.Message() == nil
}

// setParam sets a param's field to v; unless keepZero is true (for an
// explicitly given proto3 optional scalar), a zero v leaves it unset and
// makes no message on the way to it.
func setParam(req protoreflect.Message, p *spec.Param, v any, keepZero bool) error {
	if !keepZero && zero(v) {
		return nil
	}
	if p.Every != "" {
		if words, ok := v.([]string); ok && slices.Contains(words, p.Every) {
			var all []any
			for _, w := range p.Words() {
				all = append(all, w.Number)
			}
			v = all
		}
	}
	return setField(fieldPath(req, p, true), p.Field(), v, keepZero)
}

// setField sets msg's field f to v, a Go value of f's type: a scalar of any
// Go numeric type, an enum as its number or word, a message of any Go type
// for it, or a slice of them for a repeated field. Unless keepZero is set, a
// zero v leaves it unset.
func setField(msg protoreflect.Message, f protoreflect.FieldDescriptor, v any, keepZero bool) error {
	if f.IsList() {
		rv := reflect.ValueOf(v)
		if !rv.IsValid() {
			return nil
		}
		if rv.Kind() != reflect.Slice {
			return fmt.Errorf("want a list, got %T", v)
		}
		if rv.Len() == 0 {
			msg.Clear(f)
			return nil
		}
		l := msg.NewField(f).List()
		for i := range rv.Len() {
			e, err := scalar(f, rv.Index(i).Interface(), l.NewElement)
			if err != nil {
				return err
			}
			l.Append(e)
		}
		msg.Set(f, protoreflect.ValueOfList(l))
		return nil
	}
	// A zero value is no value: proto3's absence, and a oneof's member is
	// set only by a value. Explicit zero on a proto3 optional scalar still
	// sets presence when keepZero is true.
	if !keepZero && zero(v) {
		return nil
	}
	val, err := scalar(f, v, func() protoreflect.Value { return msg.NewField(f) })
	if err != nil {
		return err
	}
	msg.Set(f, val)
	return nil
}

// scalar is one value of field f.
func scalar(f protoreflect.FieldDescriptor, v any, newMessage func() protoreflect.Value) (protoreflect.Value, error) {
	rv := reflect.ValueOf(v)
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		m, ok := v.(proto.Message)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want a %s, got %T", f.Message().FullName(), v)
		}
		out := newMessage()
		return out, assign(out.Message(), m)
	case protoreflect.EnumKind:
		if s, ok := v.(string); ok {
			n, err := enumNumber(f.Enum(), s)
			return protoreflect.ValueOfEnum(n), err
		}
		if n, ok := v.(protoreflect.EnumNumber); ok {
			return protoreflect.ValueOfEnum(n), nil
		}
		n, err := integer(rv, math.MinInt32, math.MaxInt32)
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), err
	case protoreflect.BoolKind:
		b, ok := v.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want true or false, got %T", v)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.StringKind:
		s, ok := v.(string)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want a string, got %T", v)
		}
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		b, ok := v.([]byte)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want bytes, got %T", v)
		}
		return protoreflect.ValueOfBytes(b), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := integer(rv, math.MinInt32, math.MaxInt32)
		return protoreflect.ValueOfInt32(int32(n)), err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := integer(rv, math.MinInt64, math.MaxInt64)
		return protoreflect.ValueOfInt64(n), err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := integer(rv, 0, math.MaxUint32)
		return protoreflect.ValueOfUint32(uint32(n)), err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if rv.CanUint() {
			return protoreflect.ValueOfUint64(rv.Uint()), nil
		}
		n, err := integer(rv, 0, math.MaxInt64)
		return protoreflect.ValueOfUint64(uint64(n)), err
	case protoreflect.FloatKind:
		x, err := float(rv)
		return protoreflect.ValueOfFloat32(float32(x)), err
	case protoreflect.DoubleKind:
		x, err := float(rv)
		return protoreflect.ValueOfFloat64(x), err
	}
	return protoreflect.Value{}, fmt.Errorf("a %s is no param", f.Kind())
}

// integer is an integer of any Go type, or a whole float (JSON's numbers),
// in [lo, hi].
func integer(rv reflect.Value, lo, hi int64) (int64, error) {
	var n int64
	switch {
	case rv.CanInt():
		n = rv.Int()
	case rv.CanUint():
		if rv.Uint() > math.MaxInt64 {
			return 0, fmt.Errorf("%d is out of range", rv.Uint())
		}
		n = int64(rv.Uint())
	case rv.CanFloat() && rv.Float() == math.Trunc(rv.Float()):
		n = int64(rv.Float())
	default:
		return 0, fmt.Errorf("want a whole number, got %v", rv)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d is out of range", n)
	}
	return n, nil
}

func float(rv reflect.Value) (float64, error) {
	switch {
	case rv.CanFloat():
		return rv.Float(), nil
	case rv.CanInt():
		return float64(rv.Int()), nil
	case rv.CanUint():
		return float64(rv.Uint()), nil
	}
	return 0, fmt.Errorf("want a number, got %v", rv)
}

// enumNumber is an enum's value by its word ("open") or name
// (QUESTION_STATE_OPEN).
func enumNumber(e protoreflect.EnumDescriptor, s string) (protoreflect.EnumNumber, error) {
	if v := e.Values().ByName(protoreflect.Name(s)); v != nil {
		return v.Number(), nil
	}
	var words []string
	for _, w := range spec.EnumWords(e) {
		if w.Word == s {
			return w.Number, nil
		}
		words = append(words, w.Word)
	}
	return 0, fmt.Errorf("%q is not one of %s", s, strings.Join(words, ", "))
}
