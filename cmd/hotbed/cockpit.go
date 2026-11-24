package main

import (
	"fmt"
	"net/url"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// cockpit is the controller's UI, which has a page for each session,
// workspace, command and node. Every such object a command prints carries its
// page's URL (cockpit_url), made here from the address the CLI calls the
// controller at: the controller does not know the name it is reached by.
type cockpit struct {
	// base is "http://HOST:PORT"; empty when the command calls no
	// controller, and then nothing has a page.
	base string
}

func newCockpit(controllerAddr string) (cockpit, error) {
	base, err := api.ControllerBaseURL(controllerAddr)
	if err != nil {
		return cockpit{}, err
	}
	return cockpit{base: base}, nil
}

// home is the cockpit's front page.
func (c cockpit) home() string { return c.base + "/" }

func (c cockpit) session(id string) string   { return c.page("sessions", id) }
func (c cockpit) workspace(id string) string { return c.page("workspaces", id) }
func (c cockpit) command(id string) string   { return c.page("commands", id) }
func (c cockpit) node(hostname string) string {
	return c.page("nodes", hostname)
}

func (c cockpit) page(kind, id string) string {
	if c.base == "" || id == "" {
		return ""
	}
	return c.base + "/" + kind + "/" + url.PathEscape(id)
}

// pages is, for each message that has a cockpit_url, the kind of page it
// has and the field that names it. They go by name, not Go type, for the
// messages of commands made from the API's descriptors at run time.
var pages = map[protoreflect.FullName]struct{ kind, key protoreflect.Name }{
	fullName(&pb.Session{}):          {"sessions", "id"},
	fullName(&pb.Workspace{}):        {"workspaces", "id"},
	fullName(&pb.SessionWorkspace{}): {"workspaces", "id"},
	fullName(&pb.Command{}):          {"commands", "id"},
	fullName(&pb.FinishedCommand{}):  {"commands", "id"},
	fullName(&pb.Node{}):             {"nodes", "hostname"},
	fullName(&pb.Memory{}):           {"memories", "id"},
}

func fullName(m proto.Message) protoreflect.FullName { return m.ProtoReflect().Descriptor().FullName() }

// pageOf is the page of a message that has a cockpit_url; ok is false for a
// message the cockpit has no page for.
func (c cockpit) pageOf(m protoreflect.Message) (page string, ok bool) {
	p, ok := pages[m.Descriptor().FullName()]
	if !ok {
		return "", false
	}
	fd := m.Descriptor().Fields().ByName(p.key)
	v := m.Get(fd)
	var key string
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Int64Kind:
		if n := v.Int(); n > 0 {
			key = fmt.Sprint(n)
		}
	default:
		key = v.String()
	}
	return c.page(string(p.kind), key), true
}

// fill sets cockpit_url on every message in data that has one, when data is
// a protobuf message; other data makes its own URLs.
func (c cockpit) fill(data any) {
	if m, ok := data.(proto.Message); ok && c.base != "" {
		c.fillMessage(m.ProtoReflect())
	}
}

func (c cockpit) fillMessage(m protoreflect.Message) {
	if f := m.Descriptor().Fields().ByName("cockpit_url"); f != nil {
		page, ok := c.pageOf(m)
		if !ok {
			panic(fmt.Sprintf("%s has a cockpit_url but cockpit.pageOf gives it no page", m.Descriptor().FullName()))
		}
		if page != "" {
			m.Set(f, protoreflect.ValueOfString(page))
		}
	}
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case f.IsMap():
			if f.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					c.fillMessage(mv.Message())
					return true
				})
			}
		case f.IsList():
			if f.Message() != nil {
				for i, l := 0, v.List(); i < l.Len(); i++ {
					c.fillMessage(l.Get(i).Message())
				}
			}
		case f.Message() != nil:
			c.fillMessage(v.Message())
		}
		return true
	})
}
