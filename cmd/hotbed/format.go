package main

import (
	"fmt"
	"io"
	"sync"
	"text/template"

	"github.com/itchyny/gojq"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// outputOpts controls how a command's result or failure is printed.
type outputOpts struct {
	JSON bool
	JQ   string
}

func (o outputOpts) machine() bool { return o.JSON || o.JQ != "" }

// formatSpec is a hand-written command's plain-text template and the type
// it renders: a zero value of what the command's Exec returns, which
// TestFormatTemplatesNameRealFields fills in to check every field the
// template names exists.
type formatSpec struct {
	out any
	src string
}

var defaultFormatSpecs = map[string]formatSpec{
	"version":         {versionResult{}, `{{.version}}{{if .commit}} ({{short 12 .commit}}){{end}}  {{.os}}/{{.arch}}`},
	"session start":   {&pb.Session{}, "{{.id}}{{if .project}}  project={{.project}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}{{if .reminder}}\n{{.reminder}}{{end}}"},
	"session current": {sessionBinding{}, `{{.id}}  ({{.source}})`},
	"session end":     {&pb.Session{}, `{{.id}}  ended`},
	"session ls":      {&pb.ListSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}"},
	"session search":  {&pb.SearchSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}{{if .more}}(+{{.more}} older matching, not shown: --limit 0 for every one)\n{{end}}"},
	"stop":            {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"pause":           {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"resume":          {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"step ls":         {&pb.ListStepsResponse{}, "{{range .steps}}{{with .step}}{{pad 20 .name}}  {{if .everyPull}}every-pull{{else}}once      {{end}}  {{short 50 .script}}{{end}}\n{{end}}"},
	"review ls":       {&pb.ListReviewsResponse{}, "{{range .reviews}}#{{pad 4 .id}}  {{pad 12 (state .state)}}  {{pad 18 (or .model \"-\")}}  {{short 60 .title}}\n{{end}}"},
	"skills list":     {map[string][]string{"skills": nil}, "{{range .skills}}{{.}}\n{{end}}"},
	"skills get":      {map[string]string{"name": "", "content": ""}, `{{.content}}`},
	"skills install":  {map[string]any{"dir": "", "files": []string{}}, `installed {{len .files}} files in {{.dir}}`},
}

var compiledFormats = sync.OnceValue(func() map[string]*template.Template {
	out := make(map[string]*template.Template, len(defaultFormatSpecs))
	for name, f := range defaultFormatSpecs {
		t, err := apicmd.CompileFormat(f.src)
		if err != nil {
			panic(fmt.Sprintf("invalid format template for %q: %v", name, err))
		}
		out[name] = t
	}
	return out
})

func formatTemplate(name string, cmd command) *template.Template {
	if ac, ok := cmd.(*apiCommand); ok && ac.Command.FormatTmpl != nil {
		return ac.Command.FormatTmpl
	}
	return compiledFormats()[name]
}

func writeOutput(w io.Writer, name string, cmd command, data any, opts outputOpts) error {
	if opts.JQ != "" {
		q, err := gojq.Parse(opts.JQ)
		if err != nil {
			return api.Errorf(api.CodeInvalidArgs, "invalid --jq expression: %v", err)
		}
		return apicmd.FormatJQ(w, q, data)
	}
	if opts.JSON {
		writeEnvelope(w, success(name, data))
		return nil
	}
	if res, ok := asDiagnosticsResponse(data); ok {
		_, err := io.WriteString(w, formatCommandDiagnosticsReport(res))
		return err
	}
	return apicmd.FormatPlain(w, formatTemplate(name, cmd), data)
}

func asDiagnosticsResponse(data any) (*pb.GetCommandDiagnosticsResponse, bool) {
	if res, ok := data.(*pb.GetCommandDiagnosticsResponse); ok {
		return res, true
	}
	msg, ok := data.(proto.Message)
	if !ok {
		return nil, false
	}
	want := (&pb.GetCommandDiagnosticsResponse{}).ProtoReflect().Descriptor().FullName()
	if msg.ProtoReflect().Descriptor().FullName() != want {
		return nil, false
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return nil, false
	}
	res := &pb.GetCommandDiagnosticsResponse{}
	if err := proto.Unmarshal(b, res); err != nil {
		return nil, false
	}
	return res, true
}

func writeFailure(stdout, stderr io.Writer, name string, e *api.Error, opts outputOpts) {
	if opts.machine() {
		writeEnvelope(stdout, failure(name, e))
		return
	}
	fmt.Fprintf(stderr, "error: %s\n", e.Message)
	if e.Suggestion != "" {
		fmt.Fprintf(stderr, "  %s\n", e.Suggestion)
	}
}
