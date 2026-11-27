package apicmd

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/alecthomas/kong"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// Kong is the schema's commands as kong commands: a struct type for each,
// made at run time, whose fields are its flags and arguments.
type Kong struct {
	roots  []*kongNode
	leaves map[reflect.Type]*kongLeaf
}

type kongNode struct {
	name, help string
	aliases    []string
	children   []*kongNode
	leaf       *kongLeaf
	typ        reflect.Type
}

// kongLeaf is a command's struct type, field by field.
type kongLeaf struct {
	cmd    *Command
	fields []kongField
}

// kongField is what one field of a command's struct is.
type kongField struct {
	param   *spec.Param // a request field's flag or argument
	fileOf  *spec.Param // or the file flag of one
	session bool        // or -s
	file    bool        // or --file
}

// Parsed is a command kong selected, with what it was given.
type Parsed struct {
	Command *Command
	Args    Args
	// File is --file's value: where to read the command's JSON instead of
	// stdin.
	File string
	// Files is the file each file flag given names, by its param.
	Files map[*spec.Param]string
}

// Load reads what Parsed names to read: each file flag's file into its
// param, and the command's JSON from --file or stdin, unless what stands
// in for it was given. read reads a file, "-" for stdin; nil for none.
func (p *Parsed) Load(read func(path string) ([]byte, error)) error {
	for param, path := range p.Files {
		if _, ok := p.Args.Given[param]; ok {
			return api.Errorf(api.CodeInvalidArgs, "give --%s or --%s, not both", param.Name, param.FileFlag).WithSuggestion(helpOf(p.Command))
		}
		b, err := read(path)
		if err != nil {
			return api.Errorf(api.CodeInvalidArgs, "--%s: %v", param.FileFlag, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			return api.Errorf(api.CodeInvalidArgs, "--%s %s is empty", param.FileFlag, path).WithSuggestion(helpOf(p.Command))
		}
		p.Args.Given[param] = string(b)
	}
	in := p.Command.Input
	if in == nil {
		return nil
	}
	if in.Unless != nil {
		if _, ok := p.Args.Given[in.Unless]; ok {
			if p.File != "" {
				return api.Errorf(api.CodeInvalidArgs, "give the JSON (--file) or --%s, not both", flagOf(in.Unless)).WithSuggestion(helpOf(p.Command))
			}
			return nil
		}
	}
	path, where := "-", "on stdin"
	if p.File != "" {
		path, where = p.File, "in "+p.File
	}
	b, err := read(path)
	if err != nil {
		return api.Errorf(api.CodeInvalidArgs, "read the JSON: %v", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return api.Errorf(api.CodeInvalidArgs, "no JSON %s", where).WithSuggestion("pipe the command's JSON in, or pass --file FILE: " + helpOf(p.Command))
	}
	p.Args.Input = b
	return nil
}

// flagOf names how a param is given on the command line: its file flag,
// when it has one, is what stands in for a file.
func flagOf(p *spec.Param) string {
	if p.FileFlag != "" {
		return p.FileFlag
	}
	return p.Name
}

// Kong makes the schema's commands' kong types.
func (s *Schema) Kong() (*Kong, error) {
	k := &Kong{leaves: map[reflect.Type]*kongLeaf{}}
	root := &kongNode{}
	for _, c := range s.Commands {
		n := root
		for i, w := range c.Words {
			var next *kongNode
			for _, ch := range n.children {
				if ch.name == w {
					next = ch
				}
			}
			if next == nil {
				next = &kongNode{name: w, help: s.Groups[strings.Join(c.Words[:i+1], " ")]}
				n.children = append(n.children, next)
			}
			n = next
		}
		n.aliases = c.Aliases
		leaf, err := leafType(c)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name(), err)
		}
		n.leaf, n.help, n.typ = leaf, c.Help, leaf.typ()
		if other, ok := k.leaves[n.typ]; ok {
			return nil, fmt.Errorf("%s: its struct type is %s's too, so kong would run one for the other", c.Name(), other.cmd.Name())
		}
		k.leaves[n.typ] = leaf
	}
	for _, n := range root.children {
		groupType(n)
	}
	k.roots = root.children
	return k, nil
}

// Names is the top-level words of the schema's commands.
func (k *Kong) Names() []string {
	var out []string
	for _, n := range k.roots {
		out = append(out, n.name)
	}
	return out
}

// Options adds the commands to a kong parser, with fresh values to parse
// into.
func (k *Kong) Options() []kong.Option {
	var out []kong.Option
	for _, n := range k.roots {
		out = append(out, kong.DynamicCommand(n.name, n.help, "", reflect.New(n.typ).Interface()))
	}
	return out
}

// Selected is the command kong selected and what it was given; ok is false
// for a command not of the schema.
func (k *Kong) Selected(n *kong.Node) (p Parsed, ok bool) {
	if n == nil {
		return Parsed{}, false
	}
	leaf, ok := k.leaves[n.Target.Type()]
	if !ok {
		return Parsed{}, false
	}
	p = Parsed{Command: leaf.cmd, Args: Args{Given: map[*spec.Param]any{}}, Files: map[*spec.Param]string{}}
	// What was given, from the command line or the environment, even if
	// zero: --x=false is given.
	set := map[string]bool{}
	for _, f := range n.Flags {
		set[f.Name] = f.Set
	}
	for _, a := range n.Positional {
		set[a.Name] = a.Set
	}
	for i, f := range leaf.fields {
		v := n.Target.Field(i)
		switch {
		case f.session:
			p.Args.Session = v.String()
		case f.file:
			p.File = v.String()
		case f.fileOf != nil:
			if set[f.fileOf.FileFlag] && v.String() != "" {
				p.Files[f.fileOf] = v.String()
			}
		case set[f.param.Name] && !noWord(f.param, v):
			p.Args.Given[f.param] = v.Interface()
		}
	}
	return p, true
}

// noWord says whether v is an enum flag's "", its default, not a word given.
func noWord(p *spec.Param, v reflect.Value) bool {
	return v.Kind() == reflect.String && v.String() == "" && p.Field().Enum() != nil && !p.Field().IsList() && !p.Text()
}

func groupType(n *kongNode) reflect.Type {
	if n.leaf != nil {
		return n.typ
	}
	var fields []reflect.StructField
	for i, ch := range n.children {
		kv := []string{"cmd", "", "name", ch.name, "help", ch.help}
		if len(ch.aliases) > 0 {
			kv = append(kv, "aliases", strings.Join(ch.aliases, ","))
		}
		fields = append(fields, reflect.StructField{
			Name: fmt.Sprintf("C%d", i),
			Type: groupType(ch),
			Tag:  tags(kv...),
		})
	}
	n.typ = reflect.StructOf(fields)
	return n.typ
}

func (l *kongLeaf) typ() reflect.Type {
	var fields []reflect.StructField
	for i, f := range l.fields {
		sf := reflect.StructField{Name: fmt.Sprintf("F%d", i)}
		switch {
		case f.session:
			sf.Type = reflect.TypeFor[string]()
			sf.Tag = tags("name", "session", "short", "s", "env", "HOTBED_SESSION",
				"help", "The session's name (default: HOTBED_SESSION, or the session started for T3CODE_THREAD_ID / this git worktree).")
		case f.file:
			sf.Type = reflect.TypeFor[string]()
			sf.Tag = tags("name", "file", "type", "existingfile", "help", "Read the JSON from this file instead of stdin.")
		case f.fileOf != nil:
			sf.Type = reflect.TypeFor[string]()
			sf.Tag = tags("name", f.fileOf.FileFlag, "type", "existingfile", "placeholder", "FILE",
				"help", fmt.Sprintf("Read --%s from this file (- for stdin).", f.fileOf.Name))
		default:
			sf.Type, sf.Tag = paramField(f.param)
		}
		fields = append(fields, sf)
	}
	// reflect.StructOf makes one type of fields that are alike, and a leaf
	// is found by its type: a field kong skips, naming the command, makes
	// each command's type its own. It goes last, so the fields above keep
	// their indexes.
	fields = append(fields, reflect.StructField{Name: "Command", Type: reflect.TypeFor[struct{}](), Tag: tags("kong", "-", "command", l.cmd.Name())})
	return reflect.StructOf(fields)
}

// leafType lays out a command's struct: its params given on the command
// line, -s and --file.
func leafType(c *Command) (*kongLeaf, error) {
	l := &kongLeaf{cmd: c}
	for _, p := range c.Params {
		if p.Hidden || p.Message() {
			continue // filled in, or given as JSON
		}
		if t, _ := paramField(p); t == nil {
			return nil, fmt.Errorf("%s is a %s, which the command line cannot give", p.Field().FullName(), p.Field().Kind())
		}
		l.fields = append(l.fields, kongField{param: p})
		if p.FileFlag != "" {
			l.fields = append(l.fields, kongField{fileOf: p})
		}
	}
	if c.Session {
		l.fields = append(l.fields, kongField{session: true})
	}
	if c.Input != nil {
		l.fields = append(l.fields, kongField{file: true})
	}
	return l, nil
}

// paramField is a param's Go type and kong tags; a nil type for one the
// command line cannot give.
func paramField(p *spec.Param) (reflect.Type, reflect.StructTag) {
	f := p.Field()
	var t reflect.Type
	kv := []string{"help", p.Help}
	switch {
	case p.Switch != "":
		return reflect.TypeFor[bool](), tags(append(kv, "name", p.Name)...)
	case p.Text():
		t = reflect.TypeFor[string]()
	default:
		t, kv = scalarField(p, kv)
		if t == nil {
			return nil, ""
		}
	}
	if f.IsList() {
		t = reflect.SliceOf(t)
	}
	kv = append(kv, "name", p.Name)
	if f.Kind() != protoreflect.BoolKind || p.Text() {
		kv = append(kv, "placeholder", strings.ToUpper(p.Name))
	}
	if p.Positional {
		kv = append(kv, "arg", "")
		if (f.IsList() || p.Optional) && !p.Required {
			kv = append(kv, "optional", "")
		}
		if p.Passthrough {
			kv = append(kv, "passthrough", "")
		}
	} else if p.Required {
		kv = append(kv, "required", "")
	}
	if p.Short != "" {
		kv = append(kv, "short", p.Short)
	}
	if o := f.ContainingOneof(); o != nil && !o.IsSynthetic() {
		kv = append(kv, "xor", string(o.Name()))
	}
	return t, tags(kv...)
}

// scalarField is the Go type of one value of a param given as its field's
// own, with the tags its type adds to kv; a nil type for none.
func scalarField(p *spec.Param, kv []string) (reflect.Type, []string) {
	var t reflect.Type
	switch p.Field().Kind() {
	case protoreflect.BoolKind:
		t = reflect.TypeFor[bool]()
	case protoreflect.StringKind:
		t = reflect.TypeFor[string]()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		t = reflect.TypeFor[int32]()
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		t = reflect.TypeFor[int64]()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		t = reflect.TypeFor[uint32]()
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		t = reflect.TypeFor[uint64]()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		t = reflect.TypeFor[float64]()
	case protoreflect.EnumKind:
		t = reflect.TypeFor[string]()
		var words, accepted []string
		for _, w := range p.Words() {
			words = append(words, w.Word)
			accepted = append(accepted, w.Word)
			if dashed := strings.ReplaceAll(w.Word, "_", "-"); dashed != w.Word {
				accepted = append(accepted, dashed) // in-progress, as in_progress
			}
		}
		if p.Every != "" {
			words = append(words, p.Every)
			accepted = append(accepted, p.Every)
		}
		// Kong checks a given word; an empty one is none given.
		kv = append(kv, "enum", strings.Join(append(accepted, ""), ","), "default", "")
		kv[1] = strings.TrimSpace(kv[1] + " One of: " + strings.Join(words, ", ") + ".")
	default:
		return nil, kv
	}
	return t, kv
}

// tags makes a struct tag of key-value pairs.
func tags(kv ...string) reflect.StructTag {
	var parts []string
	for i := 0; i < len(kv); i += 2 {
		parts = append(parts, kv[i]+":"+strconv.Quote(kv[i+1]))
	}
	return reflect.StructTag(strings.Join(parts, " "))
}
