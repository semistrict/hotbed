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
	"github.com/semistrict/hotbed/internal/daemon"
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
	"session start":   {&pb.Session{}, "{{.id}}{{if .project}}  project={{.project}}{{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}{{with .memories}}{{if or .always .onDemand}}\n\nMemories ({{len .always}} always-on, {{len .onDemand}} on-demand — fetch details with `hotbed memory get SLUG`):{{if .always}}\nAlways-on:{{range .always}}\n  [{{if .project}}{{.project}}{{else}}global{{end}}{{if .repo}}/{{.repo}}{{end}}] {{.slug}}: {{.summary}}{{end}}{{end}}{{if .omittedAlways}}\n  (+{{.omittedAlways}} more always-on memories — run `hotbed memory ls --mode always`){{end}}{{if .onDemand}}\nOn-demand (read when relevant):{{range .onDemand}}\n  [{{if .project}}{{.project}}{{else}}global{{end}}{{if .repo}}/{{.repo}}{{end}}] {{.slug}} — {{.summary}}{{end}}{{end}}{{if .omittedOnDemand}}\n  (+{{.omittedOnDemand}} more on-demand memories — run `hotbed memory ls`){{end}}{{end}}{{end}}{{if .reminder}}\n{{.reminder}}{{end}}"},
	"session current": {sessionBinding{}, `{{.id}}  ({{.source}})`},
	"session end":     {&pb.Session{}, "{{.id}}  ended{{if .reminder}}\n{{.reminder}}{{end}}"},
	"session ls":      {&pb.ListSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}"},
	"session search":  {&pb.SearchSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}{{if .more}}(+{{.more}} older matching, not shown: --limit 0 for every one)\n{{end}}"},
	"stop":            {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"pause":           {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"resume":          {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"step ls":         {&pb.ListStepsResponse{}, "{{range .steps}}{{with .step}}{{pad 20 .name}}  {{if .everyPull}}every-pull{{else}}once      {{end}}  {{short 50 .script}}{{end}}\n{{end}}"},
	"review ls":       {&pb.ListReviewsResponse{}, "{{range .reviews}}#{{pad 4 .id}}  {{pad 12 (state .state)}}  impl={{pad 18 (or .implementerModel \"-\")}}  rev={{pad 24 (or .model \"-\")}}  {{short 60 .title}}\n{{end}}"},
	"skills list":     {map[string][]string{"skills": nil}, "{{range .skills}}{{.}}\n{{end}}"},
	"skills get":      {map[string]string{"name": "", "content": ""}, `{{.content}}`},
	"skills install":   {map[string]any{"dir": "", "files": []string{}}, `installed {{len .files}} files in {{.dir}}`},
	"daemon status":    {daemon.ServiceStatus{Live: &daemon.Snapshot{}}, "installed={{.installed}}  running={{.running}}{{if .unitPath}}\nunit:  {{.unitPath}}{{end}}{{if .logPath}}\nlog:   {{.logPath}}{{end}}{{with .live}}\ncontroller: {{or .controller \"-\"}}{{with .t3Bridge}}\nt3-bridge:  {{.state}}{{if .trackedQuestions}} ({{.trackedQuestions}} tracked){{end}}{{if .pendingQuestions}} ({{.pendingQuestions}} pending){{end}}{{end}}{{with .workerVm}}\nworker-vm:  {{if .configured}}{{.hostname}} {{.state}}{{else}}unconfigured{{end}}{{end}}{{end}}"},
	"daemon install":   {daemon.ServiceStatus{}, "installed={{.installed}}  running={{.running}}{{if .unitPath}}\nunit:  {{.unitPath}}{{end}}{{if .logPath}}\nlog:   {{.logPath}}{{end}}"},
	"daemon uninstall": {daemon.ServiceStatus{}, "installed={{.installed}}  running={{.running}}{{if .unitPath}}\nunit:  {{.unitPath}}{{end}}"},
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
	if res, ok := asProtoResponse(data, func() *pb.CommandSample { return &pb.CommandSample{} }); ok {
		_, err := io.WriteString(w, formatCommandSampleReport(res))
		return err
	}
	if res, ok := asListFreshensResponse(data); ok {
		_, err := io.WriteString(w, formatFreshenListReport(res))
		return err
	}
	if res, ok := asGetFreshenResponse(data); ok {
		_, err := io.WriteString(w, formatFreshenShowReport(res))
		return err
	}
	return apicmd.FormatPlain(w, formatTemplate(name, cmd), data)
}

func asProtoResponse[T proto.Message](data any, newEmpty func() T) (T, bool) {
	if res, ok := data.(T); ok {
		return res, true
	}
	var zero T
	msg, ok := data.(proto.Message)
	if !ok {
		return zero, false
	}
	res := newEmpty()
	if msg.ProtoReflect().Descriptor().FullName() != res.ProtoReflect().Descriptor().FullName() {
		return zero, false
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return zero, false
	}
	if err := proto.Unmarshal(b, res); err != nil {
		return zero, false
	}
	return res, true
}

func asDiagnosticsResponse(data any) (*pb.GetCommandDiagnosticsResponse, bool) {
	return asProtoResponse(data, func() *pb.GetCommandDiagnosticsResponse { return &pb.GetCommandDiagnosticsResponse{} })
}

func asListFreshensResponse(data any) (*pb.ListFreshensResponse, bool) {
	return asProtoResponse(data, func() *pb.ListFreshensResponse { return &pb.ListFreshensResponse{} })
}

func asGetFreshenResponse(data any) (*pb.GetFreshenResponse, bool) {
	return asProtoResponse(data, func() *pb.GetFreshenResponse { return &pb.GetFreshenResponse{} })
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
