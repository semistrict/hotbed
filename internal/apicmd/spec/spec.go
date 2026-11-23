// Package spec reads the commands an API's descriptors declare: the
// hotbed.v1.command option on its methods and hotbed.v1.param on their
// requests' fields (see "commands and tools" in hotbed.proto). It is the
// one reading of those options, shared by the client, which makes its CLI
// and MCP tools from them at run time, and protoc-gen-hotbed-hooks, which
// makes the Go hooks they name an interface.
package spec

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Schema is every command an API declares.
type Schema struct {
	// Files is the API's descriptors.
	Files *protoregistry.Files
	// Commands in the order the API declares them.
	Commands []*Command
	// Groups is the help of each group of commands, by its words ("question").
	Groups map[string]string
	// Hooks is every Go hook the commands name, by name.
	Hooks map[string]*Hook
}

// Command is one command: an RPC, offered with some of its request's
// fields fixed.
type Command struct {
	Words  []string
	Help   string
	Method protoreflect.MethodDescriptor
	// Session: the command acts as a session (-s on the CLI).
	Session bool
	// Params are the request's fields as given or filled in, in field order,
	// a flattened message's fields in its place.
	Params []*Param
	Fixed  []Fixed
	// Input is the field the command's JSON is (nil for none); the request
	// itself when Input.Field is nil.
	Input                 *Input
	Before, After         []string
	Waits                 bool
	ReadOnly, Destructive bool
	// Format is an optional Go text/template rendering the RPC's response as
	// plain text on the CLI.
	Format string
}

// Name is the command's words: "question ls".
func (c *Command) Name() string { return strings.Join(c.Words, " ") }

// Tool is the command's MCP tool's name: "question_ls".
func (c *Command) Tool() string { return strings.Join(c.Words, "_") }

// Fixed is a request field a command sets.
type Fixed struct {
	Field protoreflect.FieldDescriptor
	Value string
}

// Input is where a command's JSON goes.
type Input struct {
	// Field is the field the JSON is; nil for the request itself.
	Field protoreflect.FieldDescriptor
	// Hook reads the JSON, when it is not simply the field's JSON.
	Hook string
}

// Param is a field of a request, or of a message flattened into it.
type Param struct {
	// Name is its flag's name: asked-by.
	Name string
	// Path leads from the request to the field: one field, or more through
	// flattened messages.
	Path       []protoreflect.FieldDescriptor
	Short      string
	Positional bool
	Default    string
	Hook       string
	Hidden     bool
	Every      string
	// Local: the client's alone, cleared before the call.
	Local bool
	// Required: the flag (or parameter) must be given.
	Required bool
	// Optional: a positional argument that may be left out.
	Optional bool
	Help     string
}

// Field is the param's field.
func (p *Param) Field() protoreflect.FieldDescriptor { return p.Path[len(p.Path)-1] }

// Var is the param's name in MCP: asked_by.
func (p *Param) Var() string { return strings.ReplaceAll(p.Name, "-", "_") }

// Message says whether the param is a message (or messages), given only as
// JSON.
func (p *Param) Message() bool { return p.Field().Message() != nil }

// Words are an enum param's words, by number: QUESTION_STATE_OPEN is
// "open". The zero value, unspecified, has none.
func (p *Param) Words() []EnumWord {
	e := p.Field().Enum()
	if e == nil {
		return nil
	}
	return EnumWords(e)
}

// EnumWord is one value of an enum, as a word.
type EnumWord struct {
	Word   string
	Number protoreflect.EnumNumber
}

// EnumWords are an enum's values as words, by number; the zero value has none.
func EnumWords(e protoreflect.EnumDescriptor) []EnumWord {
	prefix := screamingSnake(string(e.Name())) + "_"
	var out []EnumWord
	for i := range e.Values().Len() {
		v := e.Values().Get(i)
		if v.Number() == 0 {
			continue
		}
		out = append(out, EnumWord{Word: strings.ToLower(strings.TrimPrefix(string(v.Name()), prefix)), Number: v.Number()})
	}
	return out
}

// screamingSnake is QuestionState as QUESTION_STATE.
func screamingSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// HookKind is what a hook does.
type HookKind int

const (
	// FieldHook fills in a request's field.
	FieldHook HookKind = iota
	// InputHook reads a command's JSON into its field.
	InputHook
	// BeforeHook runs before the call, and may change the request.
	BeforeHook
	// AfterHook runs after it, with its response.
	AfterHook
)

// Hook is a Go hook: what it is given and gives back.
type Hook struct {
	Name    string
	Kind    HookKind
	Request protoreflect.MessageDescriptor
	// Field is what a field hook fills in; nil for another kind.
	Field protoreflect.FieldDescriptor
	// Result is the message an input hook makes; nil for another kind.
	Result protoreflect.MessageDescriptor
	// Response is the response an after hook sees; nil for another kind.
	Response protoreflect.MessageDescriptor
	// Uses is where it is named, for errors.
	Uses []string
}

func (h *Hook) same(o *Hook) bool {
	return h.Kind == o.Kind && h.Request == o.Request && h.Field == o.Field && h.Result == o.Result && h.Response == o.Response
}

// Load reads the commands of an API's descriptors, a serialized
// FileDescriptorSet with source info (for help, its comments).
func Load(descriptors []byte) (*Schema, error) {
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(descriptors, set); err != nil {
		return nil, fmt.Errorf("read the API's descriptors: %w", err)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("read the API's descriptors: %w", err)
	}
	s := &Schema{Files: files, Groups: map[string]string{}, Hooks: map[string]*Hook{}}
	names := map[string]bool{}
	var errs []string
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			svc := f.Services().Get(i)
			svcGroups, err := groupOptions(svc)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", svc.FullName(), err))
			}
			svcGroupHelp := map[string]string{}
			for _, gs := range svcGroups {
				g := strings.Join(strings.Fields(gs.GetName()), " ")
				if g == "" {
					errs = append(errs, fmt.Sprintf("%s: a group needs a name", svc.FullName()))
					continue
				}
				if _, dup := svcGroupHelp[g]; dup {
					errs = append(errs, fmt.Sprintf("%s: duplicate group %q", svc.FullName(), g))
					continue
				}
				svcGroupHelp[g] = oneParagraph(gs.GetHelp())
			}
			seenSvcGroups := map[string]bool{}
			for j := range svc.Methods().Len() {
				m := svc.Methods().Get(j)
				specs, err := commandOptions(m)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", m.FullName(), err))
					continue
				}
				for _, cs := range specs {
					c, err := readCommand(m, cs)
					if err != nil {
						errs = append(errs, fmt.Sprintf("%s (%s): %v", m.FullName(), cs.GetName(), err))
						continue
					}
					if names[c.Name()] {
						errs = append(errs, fmt.Sprintf("%s: a second command %q", m.FullName(), c.Name()))
						continue
					}
					names[c.Name()] = true
					s.Commands = append(s.Commands, c)
					for k := 1; k < len(c.Words); k++ {
						g := strings.Join(c.Words[:k], " ")
						seenSvcGroups[g] = true
						if _, ok := s.Groups[g]; !ok {
							if help, ok := svcGroupHelp[g]; ok && help != "" {
								s.Groups[g] = help
							} else {
								s.Groups[g] = firstSentence(comment(svc))
							}
						}
					}
					errs = append(errs, s.addHooks(c)...)
				}
			}
			for g := range svcGroupHelp {
				if !seenSvcGroups[g] {
					errs = append(errs, fmt.Sprintf("%s: group %q has no commands", svc.FullName(), g))
				}
			}
		}
		return true
	})
	for _, c := range s.Commands {
		if _, ok := s.Groups[c.Name()]; ok {
			errs = append(errs, fmt.Sprintf("%q is a command and a group of commands", c.Name()))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the API's commands:\n  %s", strings.Join(errs, "\n  "))
	}
	return s, nil
}

// addHooks notes the hooks a command names, and says where one is named
// twice with different signatures.
func (s *Schema) addHooks(c *Command) []string {
	var errs []string
	add := func(h *Hook) {
		h.Uses = []string{c.Name()}
		if !goIdent(h.Name) {
			errs = append(errs, fmt.Sprintf("%s: hook %q is not an exported Go name", c.Name(), h.Name))
			return
		}
		old, ok := s.Hooks[h.Name]
		switch {
		case !ok:
			s.Hooks[h.Name] = h
		case old.same(h):
			old.Uses = append(old.Uses, c.Name())
		default:
			errs = append(errs, fmt.Sprintf("%s: hook %s is named by %s too, for something else; give each its own name", c.Name(), h.Name, strings.Join(old.Uses, ", ")))
		}
	}
	req := c.Method.Input()
	for _, p := range c.Params {
		if p.Hook != "" {
			add(&Hook{Name: p.Hook, Kind: FieldHook, Request: req, Field: p.Field()})
		}
	}
	if c.Input != nil && c.Input.Hook != "" {
		result := req
		if c.Input.Field != nil {
			result = c.Input.Field.Message()
		}
		add(&Hook{Name: c.Input.Hook, Kind: InputHook, Request: req, Result: result})
	}
	for _, name := range c.Before {
		add(&Hook{Name: name, Kind: BeforeHook, Request: req})
	}
	for _, name := range c.After {
		add(&Hook{Name: name, Kind: AfterHook, Request: req, Response: c.Method.Output()})
	}
	return errs
}

func goIdent(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// hasUnknownFields reports whether m or any populated nested message (singular,
// repeated, or map value) carries unknown wire fields.
func hasUnknownFields(m protoreflect.Message) bool {
	if m == nil || !m.IsValid() {
		return false
	}
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			if fd.Message() != nil {
				list := v.List()
				for i := range list.Len() {
					if hasUnknownFields(list.Get(i).Message()) {
						found = true
						return false
					}
				}
			}
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					if hasUnknownFields(mv.Message()) {
						found = true
						return false
					}
					return true
				})
				if found {
					return false
				}
			}
		case fd.Message() != nil:
			if hasUnknownFields(v.Message()) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func groupOptions(svc protoreflect.ServiceDescriptor) ([]*pb.GroupSpec, error) {
	opts := svc.Options()
	if opts == nil || !proto.HasExtension(opts, pb.E_Group) {
		return nil, nil
	}
	specs := proto.GetExtension(opts, pb.E_Group).([]*pb.GroupSpec)
	for _, gs := range specs {
		if hasUnknownFields(gs.ProtoReflect()) {
			return nil, fmt.Errorf("unknown field in group option (from a newer API: run hotbed upgrade)")
		}
	}
	return specs, nil
}

func commandOptions(m protoreflect.MethodDescriptor) ([]*pb.CommandSpec, error) {
	opts := m.Options()
	if opts == nil || !proto.HasExtension(opts, pb.E_Command) {
		return nil, nil
	}
	specs := proto.GetExtension(opts, pb.E_Command).([]*pb.CommandSpec)
	for _, cs := range specs {
		if hasUnknownFields(cs.ProtoReflect()) {
			return nil, fmt.Errorf("unknown field in command option (from a newer API: run hotbed upgrade)")
		}
	}
	return specs, nil
}

func paramOption(f protoreflect.FieldDescriptor) (*pb.ParamSpec, error) {
	opts := f.Options()
	if opts == nil || !proto.HasExtension(opts, pb.E_Param) {
		return nil, nil
	}
	p := proto.GetExtension(opts, pb.E_Param).(*pb.ParamSpec)
	if hasUnknownFields(p.ProtoReflect()) {
		return nil, fmt.Errorf("unknown field in param option (from a newer API: run hotbed upgrade)")
	}
	return p, nil
}

func readCommand(m protoreflect.MethodDescriptor, cs *pb.CommandSpec) (*Command, error) {
	if m.IsStreamingClient() || m.IsStreamingServer() {
		return nil, fmt.Errorf("a streaming RPC is no command yet")
	}
	c := &Command{
		Words: strings.Fields(cs.GetName()), Help: cs.GetHelp(), Method: m, Session: cs.GetSession(),
		Before: cs.GetBefore(), After: cs.GetAfter(), Waits: cs.GetWaits(), ReadOnly: cs.GetReadOnly(), Destructive: cs.GetDestructive(),
		Format: cs.GetFormat(),
	}
	if len(c.Words) == 0 {
		return nil, fmt.Errorf("a command needs a name")
	}
	if c.Help == "" {
		c.Help = paragraphs(comment(m))
	}
	req := m.Input()
	field := func(name string) (protoreflect.FieldDescriptor, error) {
		f := req.Fields().ByName(protoreflect.Name(name))
		if f == nil {
			return nil, fmt.Errorf("%s has no field %q", req.FullName(), name)
		}
		return f, nil
	}
	fixed := map[protoreflect.FieldDescriptor]bool{}
	for _, fx := range cs.GetFixed() {
		f, err := field(fx.GetField())
		if err != nil {
			return nil, err
		}
		if fx.GetValue() == "" {
			return nil, fmt.Errorf("fixed %s has no value", fx.GetField())
		}
		if oneofMember(f) {
			return nil, fmt.Errorf("fixed %s is in oneof %s, whose other members it would clear when given", fx.GetField(), f.ContainingOneof().Name())
		}
		c.Fixed = append(c.Fixed, Fixed{Field: f, Value: fx.GetValue()})
		fixed[f] = true
	}
	if in := cs.GetInput(); in != nil {
		c.Input = &Input{Hook: in.GetHook()}
		if in.GetField() != "" {
			f, err := field(in.GetField())
			if err != nil {
				return nil, err
			}
			if f.Message() == nil || f.IsList() || f.IsMap() {
				return nil, fmt.Errorf("input %s is not a message", f.Name())
			}
			c.Input.Field = f
		}
	}
	params, err := readParams(req, nil, fixed)
	if err != nil {
		return nil, err
	}
	c.Params = params
	return c, c.check()
}

// readParams are msg's fields as params, under path; fixed fields are none.
func readParams(msg protoreflect.MessageDescriptor, path []protoreflect.FieldDescriptor, fixed map[protoreflect.FieldDescriptor]bool) ([]*Param, error) {
	var out []*Param
	for i := range msg.Fields().Len() {
		f := msg.Fields().Get(i)
		pj, err := paramOption(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.FullName(), err)
		}
		if fixed[f] {
			if pj.GetLocal() {
				return nil, fmt.Errorf("%s is local and fixed: a fixed field is the server's to read", f.FullName())
			}
			continue
		}
		at := append(slices.Clone(path), f)
		if pj.GetFlatten() {
			if f.Message() == nil || f.IsList() || f.IsMap() {
				return nil, fmt.Errorf("%s: only a message field flattens", f.FullName())
			}
			inner, err := readParams(f.Message(), at, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
			continue
		}
		name := pj.GetName()
		if name == "" {
			name = strings.ReplaceAll(string(f.Name()), "_", "-")
		}
		out = append(out, &Param{
			Name: name, Path: at, Short: pj.GetShort(), Positional: pj.GetPositional(),
			Default: pj.GetDefault(), Hook: pj.GetHook(), Hidden: pj.GetHidden(), Every: pj.GetEvery(), Local: pj.GetLocal(), Required: pj.GetRequired(), Optional: pj.GetOptional(), Help: oneParagraph(comment(f)),
		})
	}
	return out, nil
}

// check refuses what a command cannot mean.
func (c *Command) check() error {
	taken := map[string]string{}
	take := func(name, what string) error {
		if old, ok := taken[name]; ok {
			return fmt.Errorf("%s and %s are both --%s", old, what, name)
		}
		taken[name] = what
		return nil
	}
	if c.Session {
		if err := take("session", "the session"); err != nil {
			return err
		}
	}
	if c.Input != nil {
		if err := take("file", "the input's file"); err != nil {
			return err
		}
	}
	shorts := map[string]bool{}
	if c.Session {
		shorts["s"] = true
	}
	var optPos, listPos string
	for _, p := range c.Params {
		f := p.Field()
		what := string(f.FullName())
		switch {
		case p.Default != "" && p.Hook != "":
			return fmt.Errorf("%s has a default and a hook; give it one", what)
		case p.Hidden && p.Default == "" && p.Hook == "":
			return fmt.Errorf("%s is hidden with neither default nor hook to fill it in", what)
		case p.Hidden && (p.Positional || p.Short != ""):
			return fmt.Errorf("%s is hidden, so it is never given: no positional or short", what)
		case p.Every != "" && (f.Enum() == nil || !f.IsList()):
			return fmt.Errorf("%s: every is for a repeated enum", what)
		case p.Positional && p.Message():
			return fmt.Errorf("%s: a message is not positional", what)
		case f.IsMap():
			return fmt.Errorf("%s: a map is no param", what)
		case p.Message() && !p.Hidden && !c.inputs(p):
			return fmt.Errorf("%s is a message: flatten it, take it as the command's input, or hide it", what)
		case len(p.Short) > 1:
			return fmt.Errorf("%s: short is one letter", what)
		case p.Short == "h":
			return fmt.Errorf("%s: -h is help's", what)
		case !p.Hidden && !p.Message() && f.Kind() == protoreflect.BytesKind:
			return fmt.Errorf("%s: bytes are given as no flag", what)
		case !p.Hidden && p.Message() && len(p.Path) > 1:
			return fmt.Errorf("%s: a message in a flattened message is given no way; flatten it too, or hide it", what)
		case p.Local && p.Hidden:
			return fmt.Errorf("%s is local and hidden: a local field is given, as nothing sends it", what)
		case p.Local && len(p.Path) > 1:
			return fmt.Errorf("%s is local in a flattened message: only a request's own field is local", what)
		case p.Local && oneofMember(f):
			return fmt.Errorf("%s is local in oneof %s: cleared before the call, it would leave the oneof empty, its other members' defaults not run", what, f.ContainingOneof().Name())
		case p.Required && p.Hidden:
			return fmt.Errorf("%s is required and hidden: a hidden field is never given", what)
		case p.Required && (p.Default != "" || p.Hook != ""):
			return fmt.Errorf("%s is required with a default or hook; give it one", what)
		case p.Required && oneofMember(f):
			return fmt.Errorf("%s is required in oneof %s: a oneof member is optional", what, f.ContainingOneof().Name())
		case p.Optional && !p.Positional:
			return fmt.Errorf("%s: optional is for a positional argument", what)
		case p.Optional && p.Required:
			return fmt.Errorf("%s is both optional and required", what)
		}
		if p.Positional && !p.Hidden {
			if listPos != "" {
				return fmt.Errorf("%s: positional argument cannot follow repeated positional %s", what, listPos)
			}
			isOptional := (f.IsList() || p.Optional) && !p.Required
			if !isOptional && optPos != "" {
				return fmt.Errorf("%s: required positional argument cannot follow optional positional %s", what, optPos)
			}
			if f.IsList() {
				listPos = what
			}
			if isOptional && optPos == "" {
				optPos = what
			}
		}
		if p.Short != "" {
			if shorts[p.Short] {
				return fmt.Errorf("%s: -%s is taken", what, p.Short)
			}
			shorts[p.Short] = true
		}
		if !p.Hidden {
			if err := take(p.Name, what); err != nil {
				return err
			}
		}
	}
	return nil
}

// oneofMember says whether f is in a oneof, not proto3 optional's.
func oneofMember(f protoreflect.FieldDescriptor) bool {
	o := f.ContainingOneof()
	return o != nil && !o.IsSynthetic()
}

// inputs says whether the command's JSON gives p.
func (c *Command) inputs(p *Param) bool {
	return c.Input != nil && (c.Input.Field == nil || c.Input.Field == p.Path[0])
}

// comment is a descriptor's leading comment, as written.
func comment(d protoreflect.Descriptor) string {
	loc := d.ParentFile().SourceLocations().ByDescriptor(d)
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(loc.LeadingComments, "\n"), "\n") {
		lines = append(lines, strings.TrimPrefix(l, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// paragraphs joins the lines of each of a comment's paragraphs.
func paragraphs(s string) string {
	var out []string
	for _, p := range strings.Split(s, "\n\n") {
		out = append(out, oneParagraph(p))
	}
	return strings.Join(out, "\n\n")
}

// oneParagraph joins a comment's lines.
func oneParagraph(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstSentence is a comment's first sentence, for a group's one line.
func firstSentence(s string) string {
	s = oneParagraph(s)
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
