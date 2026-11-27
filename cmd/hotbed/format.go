package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/itchyny/gojq"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
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
	"version":          {versionResult{}, `{{.version}}{{if .commit}} ({{short 12 .commit}}){{end}}  {{.os}}/{{.arch}}`},
	"session start":    {&pb.Session{}, "{{.id}}{{if .project}}  project={{.project}}{{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}{{with .memories}}{{if or .always .onDemand}}\n\nMemories ({{len .always}} always-on, {{len .onDemand}} on-demand — fetch details with `hotbed memory get SLUG`):{{if .always}}\nAlways-on:{{range .always}}\n  [{{if .project}}{{.project}}{{else}}global{{end}}{{if .repo}}/{{.repo}}{{end}}] {{.slug}}: {{.summary}}{{end}}{{end}}{{if .omittedAlways}}\n  (+{{.omittedAlways}} more always-on memories — run `hotbed memory ls --mode always`){{end}}{{if .onDemand}}\nOn-demand (read when relevant):{{range .onDemand}}\n  [{{if .project}}{{.project}}{{else}}global{{end}}{{if .repo}}/{{.repo}}{{end}}] {{.slug}} — {{.summary}}{{end}}{{end}}{{if .omittedOnDemand}}\n  (+{{.omittedOnDemand}} more on-demand memories — run `hotbed memory ls`){{end}}{{end}}{{end}}{{if .reminder}}\n{{.reminder}}{{end}}"},
	"session current":  {sessionBinding{}, `{{.id}}  ({{.source}})`},
	"session end":      {&pb.Session{}, "{{.id}}  ended{{if .reminder}}\n{{.reminder}}{{end}}"},
	"session ls":       {&pb.ListSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}"},
	"session search":   {&pb.SearchSessionsResponse{}, "{{range .sessions}}{{pad 20 .id}}  {{pad 12 .project}}  {{if .endedAt}}ended{{else}}live {{end}}{{if .model}}  model={{.model}}{{end}}{{if .t3codeThreadId}}  t3={{.t3codeThreadId}}{{end}}\n{{end}}{{if .more}}(+{{.more}} older matching, not shown: --limit 0 for every one)\n{{end}}"},
	"stop":             {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"pause":            {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"resume":           {&pb.Command{}, `{{.id}}  {{state .state}}{{if .reason}}  {{.reason}}{{end}}`},
	"step ls":          {&pb.ListStepsResponse{}, "{{range .steps}}{{with .step}}{{pad 20 .name}}  {{if .everyPull}}every-pull{{else}}once      {{end}}  {{short 50 .script}}{{end}}\n{{end}}"},
	"skills list":      {map[string][]string{"skills": nil}, "{{range .skills}}{{.}}\n{{end}}"},
	"skills get":       {map[string]string{"name": "", "content": ""}, `{{.content}}`},
	"skills install":   {map[string]any{"dir": "", "files": []string{}}, `installed {{len .files}} files in {{.dir}}`},
	"daemon status":    {daemon.ServiceStatus{Live: &daemon.Snapshot{}}, "installed={{.installed}}  running={{.running}}{{if .unitPath}}\nunit:  {{.unitPath}}{{end}}{{if .logPath}}\nlog:   {{.logPath}}{{end}}{{with .live}}\ncontroller: {{or .controller \"-\"}}{{with .t3Bridge}}\nt3-bridge:  {{.state}}{{if .trackedQuestions}} ({{.trackedQuestions}} tracked){{end}}{{if .pendingQuestions}} ({{.pendingQuestions}} pending){{end}}{{end}}{{with .worker}}\nworker:     {{if .configured}}{{.state}}{{if .detail}} ({{.detail}}){{end}}{{else}}unconfigured{{end}}{{end}}{{end}}"},
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
	if data == nil {
		return nil
	}
	text, ok, err := report(data, wantsOutputTimestamps(cmd))
	if err != nil {
		return err
	}
	if ok {
		_, err = io.WriteString(w, text)
		return err
	}
	return apicmd.FormatPlain(w, formatTemplate(name, cmd), data)
}

func wantsOutputTimestamps(cmd command) bool {
	ac, ok := cmd.(*apiCommand)
	if !ok {
		return false
	}
	for p, v := range ac.Args.Given {
		if p.Name == "timestamps" {
			b, _ := v.(bool)
			return b
		}
	}
	return false
}

// formatLogTimestamp renders a unix-millisecond received timestamp in local
// wall time: "15:04:05.000" today (12 chars), "2006-01-02 15:04:05.000" for
// earlier days, or "-           " (12 chars) when unknown (0).
func formatLogTimestamp(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-           "
	}
	t := time.UnixMilli(ms).Local()
	if y, m, d := now.Date(); t.Year() == y && t.Month() == m && t.Day() == d {
		return t.Format("15:04:05.000")
	}
	return t.Format("2006-01-02 15:04:05.000")
}

func formatOutputQueryWithTimestamps(res *pb.OutputQuery, now time.Time) string {
	var b strings.Builder
	for _, l := range res.Lines {
		sep := "  "
		if l.Match {
			sep = ": "
		}
		fmt.Fprintf(&b, "%-6d  %s%s%s\n", l.Number, formatLogTimestamp(l.ReceivedAt, now), sep, l.Text)
	}
	fmt.Fprintf(&b, "[%s %s", res.CommandId, enumWord(res.State))
	if res.ExitCode != nil && *res.ExitCode != 0 {
		fmt.Fprintf(&b, " exit %d", *res.ExitCode)
	}
	fmt.Fprintf(&b, " · %d lines", res.TotalLines)
	if res.Matches != nil && *res.Matches > 0 {
		fmt.Fprintf(&b, " · %d matching", *res.Matches)
	}
	if res.Waited != "" {
		fmt.Fprintf(&b, " · wait %s", res.Waited)
	}
	if res.NextLine > 0 {
		fmt.Fprintf(&b, " · next --after %d", res.NextLine)
	}
	b.WriteByte(']')
	if res.Omitted != "" {
		fmt.Fprintf(&b, "\n[%s]", res.Omitted)
	}
	b.WriteByte('\n')
	return b.String()
}

// report renders data as a plain-text report written in Go, where a
// template would not do; ok is false for data that has none. The output
// timestamps its lines when asked (--timestamps).
func report(data any, timestamps bool) (text string, ok bool, err error) {
	if res, ok := data.(boardImportSummary); ok {
		return formatBoardImportSummary(res), true, nil
	}
	msg, ok := data.(proto.Message)
	if !ok {
		return "", false, nil
	}
	switch msg.ProtoReflect().Descriptor().FullName() {
	case fullName(&pb.GetCommandDiagnosticsResponse{}):
		return render(msg, formatCommandDiagnosticsReport)
	case fullName(&pb.CommandSample{}):
		return render(msg, formatCommandSampleReport)
	case fullName(&pb.ListFreshensResponse{}):
		return render(msg, formatFreshenListReport)
	case fullName(&pb.GetFreshenResponse{}):
		return render(msg, formatFreshenShowReport)
	case fullName(&pb.ListBoardThreadsResponse{}):
		return render(msg, formatBoardThreadsReport)
	case fullName(&pb.GetBoardThreadResponse{}):
		return render(msg, formatBoardThreadReport)
	case fullName(&pb.ListBoardTasksResponse{}):
		return render(msg, formatBoardTasksReport)
	case fullName(&pb.ListBoardEventsResponse{}):
		return render(msg, formatBoardEventsReport)
	case fullName(&pb.OutputQuery{}):
		if timestamps {
			return render(msg, func(res *pb.OutputQuery) string { return formatOutputQueryWithTimestamps(res, time.Now()) })
		}
	}
	return "", false, nil
}

// render is f's report of msg, as f's Go type.
func render[T proto.Message](msg proto.Message, f func(T) string) (string, bool, error) {
	res, err := asType[T](msg)
	if err != nil {
		return "", false, err
	}
	return f(res), true, nil
}

// asType is msg, a message of T's name, as T: msg itself, or (for a
// response of a command made at run time) decoded into it. One that will
// not decode is this client's schema out of step with the controller's,
// which fails rather than print as something else.
func asType[T proto.Message](msg proto.Message) (T, error) {
	if res, ok := msg.(T); ok {
		return res, nil
	}
	var zero T
	res := zero.ProtoReflect().Type().New().Interface().(T)
	b, err := proto.Marshal(msg)
	if err == nil {
		err = proto.Unmarshal(b, res)
	}
	if err != nil {
		return zero, api.Errorf(api.CodeInternal, "read the %s response as this client's: %v", msg.ProtoReflect().Descriptor().FullName(), err).
			WithSuggestion("run hotbed upgrade: the controller's API may be newer than this client")
	}
	return res, nil
}

// enumWord is an enum value as a word: COMMAND_STATE_RUNNING is
// "running", NOT_STARTED "not_started"; "unknown" for its zero value, or
// one this client does not know.
func enumWord(e protoreflect.Enum) string {
	for _, w := range spec.EnumWords(e.Descriptor()) {
		if w.Number == e.Number() {
			return w.Word
		}
	}
	return "unknown"
}

// formatBytes is a byte count for people: 512 B, 1.5 KiB, 2.0 MiB, 1.25 GiB;
// negative for a count that fell.
func formatBytes(b int64) string {
	if b < 0 {
		return "-" + formatBytes(-b)
	}
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
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
