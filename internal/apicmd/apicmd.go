// Package apicmd runs the commands an API's descriptors declare (package
// spec reads them): it builds each one's request from what was given, the
// command's fixed values, defaults and Go hooks, calls the RPC, and renders
// the commands as the CLI's (kong) and as MCP tools. Nothing in it knows
// hotbed's commands: they are all in the descriptors and the hooks.
package apicmd

import (
	"context"
	"fmt"
	"slices"
	"text/template"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// Schema is an API's commands, ready to run.
type Schema struct {
	*spec.Schema
	Commands []*Command
	byName   map[string]*Command
}

// Command is a command with its expressions and output template compiled.
type Command struct {
	*spec.Command
	fixed    []expr               // by spec.Command.Fixed
	defaults map[*spec.Param]expr // by param, for those with a default
	// FormatTmpl is the compiled plain-text output template, or nil.
	FormatTmpl *template.Template
}

// Load reads an API's commands from its descriptors (a FileDescriptorSet
// with source info) and compiles their defaults and output templates,
// failing on any expression or template that does not compile.
func Load(descriptors []byte) (*Schema, error) {
	ss, err := spec.Load(descriptors)
	if err != nil {
		return nil, err
	}
	s := &Schema{Schema: ss, byName: map[string]*Command{}}
	var errs []error
	for _, sc := range ss.Commands {
		c, err := compile(sc)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sc.Name(), err))
			continue
		}
		if sc.Format != "" {
			tmpl, err := CompileFormat(sc.Format)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: format template: %w", sc.Name(), err))
				continue
			}
			c.FormatTmpl = tmpl
		}
		s.Commands = append(s.Commands, c)
		s.byName[c.Name()] = c
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the API's commands: %v", errs)
	}
	return s, nil
}

// Command is the command of that name ("question ls"), or nil.
func (s *Schema) Command(name string) *Command { return s.byName[name] }

// Hooks is the client's Go hooks by name, each over any message: what a
// generated Table makes of a typed Hooks interface.
type Hooks struct {
	Fields map[string]FieldHook
	Inputs map[string]InputHook
	Before map[string]BeforeHook
	After  map[string]AfterHook
}

// NewHooks is an empty table.
func NewHooks() Hooks {
	return Hooks{Fields: map[string]FieldHook{}, Inputs: map[string]InputHook{}, Before: map[string]BeforeHook{}, After: map[string]AfterHook{}}
}

type (
	// FieldHook gives a field's value, a Go value of its type.
	FieldHook func(ctx context.Context, c *Call, req proto.Message) (any, error)
	// InputHook reads a command's JSON.
	InputHook func(ctx context.Context, c *Call, input []byte) (proto.Message, error)
	// BeforeHook runs before the call and may change req.
	BeforeHook func(ctx context.Context, c *Call, req proto.Message) error
	// AfterHook runs after it.
	AfterHook func(ctx context.Context, c *Call, req, res proto.Message) error
)

// Missing is the hooks s names that h lacks: a client older than the API
// whose commands it runs.
func (h Hooks) Missing(s *spec.Schema) []string {
	var out []string
	for name, hook := range s.Hooks {
		var ok bool
		switch hook.Kind {
		case spec.FieldHook:
			_, ok = h.Fields[name]
		case spec.InputHook:
			_, ok = h.Inputs[name]
		case spec.BeforeHook:
			_, ok = h.Before[name]
		case spec.AfterHook:
			_, ok = h.After[name]
		}
		if !ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Call is a command being run, as its hooks see it.
type Call struct {
	Command  *Command
	given    string // -s's value
	sessions Sessions
}

// Session is the session the command acts as, "" for none: a hook that
// needs one fails, saying so.
func (c *Call) Session(ctx context.Context) (string, error) { return c.sessions.Session(ctx, c.given) }

// NamedSession is the session a user's change names, if any.
func (c *Call) NamedSession(ctx context.Context) (string, error) {
	return c.sessions.Named(ctx, c.given)
}

// Sessions is how a command learns the session it acts as: on the CLI,
// from -s, HOTBED_SESSION or where it runs; in MCP, the server's own.
type Sessions interface {
	// Session is the session a command acts as, "" for none: the server,
	// which knows what else would do, says what is missing. given is -s's
	// value, "" when not given.
	Session(ctx context.Context, given string) (string, error)
	// Named is the session a change the user makes names: -s's value, or
	// "" for the user's own.
	Named(ctx context.Context, given string) (string, error)
}

// fieldPath walks a param's path in msg, making the messages on the way
// when make is set; it returns the message holding the param's field, nil
// when one on the way is unset and make is not.
func fieldPath(msg protoreflect.Message, p *spec.Param, make bool) protoreflect.Message {
	for _, f := range p.Path[:len(p.Path)-1] {
		if !msg.Has(f) && !make {
			return nil
		}
		msg = msg.Mutable(f).Message()
	}
	return msg
}

// isSet says whether a param's field has a value in req.
func isSet(req protoreflect.Message, p *spec.Param) bool {
	m := fieldPath(req, p, false)
	return m != nil && m.Has(p.Field())
}

// otherMember is the member of p's oneof, other than p's field, that has a
// value in req; nil for none, or for a field in no oneof.
func otherMember(req protoreflect.Message, p *spec.Param) protoreflect.FieldDescriptor {
	o := p.Field().ContainingOneof()
	if o == nil || o.IsSynthetic() {
		return nil
	}
	m := fieldPath(req, p, false)
	if m == nil {
		return nil
	}
	if w := m.WhichOneof(o); w != nil && w != p.Field() {
		return w
	}
	return nil
}
