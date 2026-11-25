package main

import (
	"io"
	"reflect"
	"testing"
	"text/template"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/semistrict/hotbed/internal/apicmd"
)

// Every plain-text template names only fields its command's output has:
// each renders, with missing keys an error, against that output with every
// field set (every member of a oneof too, though a real output has one at
// most). A template naming a field that does not exist (a typo, or one that
// lives in a nested message) would otherwise print "<no value>" or fail only
// when a person runs the command.
func TestFormatTemplatesNameRealFields(t *testing.T) {
	for name, f := range defaultFormatSpecs {
		t.Run(name, func(t *testing.T) {
			// A map names its keys itself; anything else is filled in.
			out := f.out
			if m, ok := out.(proto.Message); ok {
				out = everyField(t, m.ProtoReflect().Descriptor())
			} else if reflect.TypeOf(out).Kind() != reflect.Map {
				out = filledGo(reflect.TypeOf(out)).Interface()
			}
			strictRender(t, compiledFormats()[name], out)
		})
	}
	schema, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range schema.Commands {
		if c.FormatTmpl == nil {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			strictRender(t, c.FormatTmpl, everyField(t, c.Method.Output()))
		})
	}
}

func strictRender(t *testing.T, tmpl *template.Template, data any) {
	t.Helper()
	strict, err := tmpl.Clone()
	if err != nil {
		t.Fatal(err)
	}
	strict.Option("missingkey=error")
	if err := apicmd.FormatPlain(io.Discard, strict, data); err != nil {
		t.Errorf("render against a filled output: %v", err)
	}
}

// everyField is a message of type md as JSON, with every field set, every
// member of each oneof included: the union of the messages filled with
// each oneof's first member, then its second, and so on.
func everyField(t *testing.T, md protoreflect.MessageDescriptor) any {
	t.Helper()
	var out any
	for choice := range oneofWidth(md, 0) {
		v, err := apicmd.ToJSONValue(filled(md, choice))
		if err != nil {
			t.Fatal(err)
		}
		out = union(out, v)
	}
	return out
}

// oneofWidth is the most members any oneof in md (and what it holds) has,
// and 1 when it has none.
func oneofWidth(md protoreflect.MessageDescriptor, depth int) int {
	n := 1
	if depth > 6 {
		return n
	}
	oneofs := md.Oneofs()
	for i := range oneofs.Len() {
		if o := oneofs.Get(i); !o.IsSynthetic() {
			n = max(n, o.Fields().Len())
		}
	}
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.IsMap() {
			fd = fd.MapValue()
		}
		if fd.Message() != nil {
			n = max(n, oneofWidth(fd.Message(), depth+1))
		}
	}
	return n
}

// union merges two JSON values: objects key by key, arrays element by
// element; otherwise a.
func union(a, b any) any {
	switch av := a.(type) {
	case nil:
		return b
	case map[string]any:
		bv, _ := b.(map[string]any)
		for k, v := range bv {
			av[k] = union(av[k], v)
		}
		return av
	case []any:
		bv, _ := b.([]any)
		for i := range min(len(av), len(bv)) {
			av[i] = union(av[i], bv[i])
		}
		return av
	}
	return a
}

// filled is a message of type md with every field set, lists and maps to one
// element, nested messages filled in turn down to a depth that ends
// recursive types; of each oneof, the member numbered choice (wrapping
// around).
func filled(md protoreflect.MessageDescriptor, choice int) proto.Message {
	m := dynamicpb.NewMessage(md)
	fill(m, 0, choice)
	return m
}

func fill(m protoreflect.Message, depth, choice int) {
	if depth > 6 {
		return
	}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if o := fd.ContainingOneof(); o != nil && !o.IsSynthetic() && o.Fields().Get(choice%o.Fields().Len()) != fd {
			continue
		}
		switch {
		case fd.IsList():
			l := m.Mutable(fd).List()
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				e := l.NewElement()
				fill(e.Message(), depth+1, choice)
				l.Append(e)
			} else {
				l.Append(scalar(fd))
			}
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			k := scalar(fd.MapKey()).MapKey()
			if v := fd.MapValue(); v.Kind() == protoreflect.MessageKind {
				e := mp.NewValue()
				fill(e.Message(), depth+1, choice)
				mp.Set(k, e)
			} else {
				mp.Set(k, scalar(v))
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			fill(m.Mutable(fd).Message(), depth+1, choice)
		default:
			m.Set(fd, scalar(fd))
		}
	}
}

func scalar(fd protoreflect.FieldDescriptor) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		return protoreflect.ValueOfEnum(vals.Get(vals.Len() - 1).Number())
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("x"))
	}
	panic("unhandled kind " + fd.Kind().String())
}

// filledGo is a Go value of type t with every field set and every slice
// holding one element.
func filledGo(t reflect.Type) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Struct:
		for i := range t.NumField() {
			if t.Field(i).IsExported() {
				v.Field(i).Set(filledGo(t.Field(i).Type))
			}
		}
	case reflect.Slice:
		v.Set(reflect.Append(v, filledGo(t.Elem())))
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		p.Elem().Set(filledGo(t.Elem()))
		v.Set(p)
	}
	return v
}
