package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestOutputModesPlainJSONAndJQ(t *testing.T) {
	t.Parallel()
	t.Run("plain text default", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"version"}, deps{here: newTestHere(t, nil).here(), stdout: &stdout, stderr: &stderr})
		if code != exitOK {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
		out := strings.TrimSpace(stdout.String())
		if strings.HasPrefix(out, "{") {
			t.Fatalf("plain text default printed JSON: %q", out)
		}
		if !strings.Contains(out, "/") {
			t.Errorf("expected os/arch in plain version output, got %q", out)
		}
	})

	t.Run("--json envelope", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"version", "--json"}, deps{here: newTestHere(t, nil).here(), stdout: &stdout, stderr: &stderr})
		if code != exitOK {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
		var env output
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("invalid JSON envelope %q: %v", stdout.String(), err)
		}
		if !env.OK || env.Command != "version" {
			t.Errorf("unexpected envelope: %+v", env)
		}
	})

	t.Run("--jq filter", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"version", "--jq", ".os"}, deps{here: newTestHere(t, nil).here(), stdout: &stdout, stderr: &stderr})
		if code != exitOK {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got == "" || strings.HasPrefix(got, "\"") {
			t.Errorf("expected raw string OS from --jq .os, got %q", got)
		}
	})

	t.Run("invalid --jq exits 2", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"version", "--jq", ".[invalid"}, deps{here: newTestHere(t, nil).here(), stdout: &stdout, stderr: &stderr})
		if code != exitInvalidArgs {
			t.Fatalf("code = %d, want %d", code, exitInvalidArgs)
		}
	})

	t.Run("plain error goes to stderr", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"no-such-command"}, deps{here: newTestHere(t, nil).here(), stdout: &stdout, stderr: &stderr})
		if code != exitInvalidArgs {
			t.Fatalf("code = %d, want %d", code, exitInvalidArgs)
		}
		if stdout.Len() != 0 {
			t.Errorf("want empty stdout on plain error, got %q", stdout.String())
		}
		if !strings.HasPrefix(stderr.String(), "error: ") {
			t.Errorf("want stderr starting with 'error: ', got %q", stderr.String())
		}
	})

	t.Run("proto command format template renders Question", func(t *testing.T) {
		schema, err := apiSchema()
		if err != nil {
			t.Fatal(err)
		}
		cmd := schema.Command("question show")
		if cmd == nil || cmd.FormatTmpl == nil {
			t.Fatal("question show missing compiled FormatTmpl")
		}
		var buf bytes.Buffer
		err = apicmd.FormatPlain(&buf, cmd.FormatTmpl, &pb.Question{
			Id:       7,
			State:    pb.QuestionState_QUESTION_STATE_OPEN,
			Revision: 2,
			Project:  "hotbed",
			Prompt:   &pb.Prompt{Question: "Which store?"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, "#7") || !strings.Contains(got, "open") || !strings.Contains(got, "rev=2") || !strings.Contains(got, "Which store?") {
			t.Errorf("unexpected rendered question: %q", got)
		}
	})
}

// Every format, cmd/hotbed's defaults and the API's, renders a response
// that leaves out the fields it names, as an older peer's does (a dev
// client's nodes, board 7259) or a new one's (a node that joined and has
// not beaten yet has no resources): each list it ranges over holds one
// empty item, and no object it reaches into is there at all.
func TestFormatsRenderMissingFields(t *testing.T) {
	t.Parallel()
	formats := map[string]*template.Template{}
	for name := range defaultFormatSpecs {
		formats[name] = compiledFormats()[name]
	}
	schema, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range schema.Commands {
		if c.FormatTmpl != nil {
			formats[c.Name()] = c.FormatTmpl
		}
	}
	ranged := regexp.MustCompile(`\{\{range \.(\w+)\}\}`)
	// What a format enters with is there, holding nothing it names (with
	// skips an empty object).
	scoped := regexp.MustCompile(`\{\{with \.(\w+)\}\}`)
	for name, tmpl := range formats {
		src := tmpl.Root.String()
		data := map[string]any{}
		for _, m := range scoped.FindAllStringSubmatch(src, -1) {
			data[m[1]] = map[string]any{"unnamed": true}
		}
		for _, m := range ranged.FindAllStringSubmatch(src, -1) {
			data[m[1]] = []any{map[string]any{}}
		}
		var out bytes.Buffer
		if err := apicmd.FormatPlain(&out, tmpl, data); err != nil {
			t.Errorf("%s's format, its fields left out: %v", name, err)
		}
	}
}

// Default formats print real newlines: a Go raw string's \n is a backslash
// and an n, which ran every row of ps onto one line (board 7237).
func TestDefaultFormatsUseRealNewlines(t *testing.T) {
	t.Parallel()
	for name, f := range defaultFormatSpecs {
		if strings.Contains(f.src, `\n`) {
			t.Errorf("%s's default format holds a literal \\n: %q", name, f.src)
		}
	}
	schema, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range schema.Commands {
		if strings.Contains(c.Format, `\n`) {
			t.Errorf("%s's proto format holds a literal \\n: %q", c.Name(), c.Format)
		}
	}
	psCmd := schema.Command("ps")
	if psCmd == nil || psCmd.FormatTmpl == nil {
		t.Fatal("ps command missing compiled FormatTmpl")
	}
	var out bytes.Buffer
	err = apicmd.FormatPlain(&out, psCmd.FormatTmpl, &pb.ListCommandsResponse{Commands: []*pb.Command{
		{Id: "a1", State: pb.CommandState_COMMAND_STATE_RUNNING, SessionId: "brave-otter", Argv: "make"},
		{Id: "b2", State: pb.CommandState_COMMAND_STATE_SUCCEEDED, SessionId: "brave-otter", Argv: "make test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "a1            running     brave-otter       make\nb2            succeeded   brave-otter       make test\n"
	if got := out.String(); got != want {
		t.Errorf("ps of two commands =\n%q\nwant\n%q", got, want)
	}
}

// review ls names each review's implementer and reviewer model(s), and says
// so when a review has none yet (requested, or legacy) (board 7788).
func TestReviewLsShowsTheModel(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := apicmd.FormatPlain(&out, compiledFormats()["review ls"], &pb.ListReviewsResponse{Reviews: []*pb.Review{
		{Id: 2, State: pb.ReviewState_REVIEW_STATE_REQUESTED, Title: "Add B"},
		{Id: 1, State: pb.ReviewState_REVIEW_STATE_DONE, Title: "Add A", ImplementerModel: "gemini-4-neon", Model: "claude-opus-5-5"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "#2     requested     impl=-                   rev=-                         Add B\n#1     done          impl=gemini-4-neon       rev=claude-opus-5-5           Add A\n"
	if got := out.String(); got != want {
		t.Errorf("review ls =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatByHandCommandsAndDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cmd    string
		state  pb.CommandState
		reason string
		want   string
	}{
		{"stop", pb.CommandState_COMMAND_STATE_CANCELLED, `stopped by laptop: "hung".`, "c1  cancelled  stopped by laptop: \"hung\".\n"},
		{"pause", pb.CommandState_COMMAND_STATE_PAUSED, `paused by session s1 on laptop: "check".`, "c1  paused  paused by session s1 on laptop: \"check\".\n"},
		{"resume", pb.CommandState_COMMAND_STATE_RUNNING, "", "c1  running\n"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			var out bytes.Buffer
			err := writeOutput(&out, tc.cmd, nil, &pb.Command{Id: "c1", State: tc.state, Reason: tc.reason}, outputOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("%s plain = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}

	t.Run("diagnostics empty", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeOutput(&out, "diagnostics", nil, &pb.GetCommandDiagnosticsResponse{}, outputOpts{}); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "(no diagnostics)\n" {
			t.Errorf("empty diagnostics = %q, want (no diagnostics)\\n", got)
		}
	})

	t.Run("diagnostics plain, json, and jq", func(t *testing.T) {
		resp := &pb.GetCommandDiagnosticsResponse{
			HasMore:      true,
			NextAfterSeq: 1,
			Diagnostics: []*pb.CommandDiagnostics{
				{
					CommandId: "cmd-1",
					Intervention: &pb.CommandIntervention{
						Seq:              1,
						Kind:             pb.InterventionKind_INTERVENTION_KIND_PAUSE,
						By:               &pb.Stamp{At: 1700000000000, Session: "deft-emu", Host: "laptop"},
						Comment:          "check locks",
						Diagnosed:        true,
						DiagnosticsError: "perf unavailable",
					},
					Sample: &pb.ProcessSample{
						CommandId: "cmd-1",
						Hostname:  "ubu1",
						Arch:      "amd64",
						Kernel:    "6.8.0",
						UnitName:  "hotbed-cmd-1.scope",
						StartedAt: 1700000000000,
						EndedAt:   1700000002000,
						DurationMs: 2000,
						Procs: []*pb.SampledProcess{
							{Pid: 42, Comm: "worker", State: "S", Cmdline: "worker --test"},
						},
					},
					Inspect: &pb.InspectCommandResponse{
						CommandId:   "cmd-1",
						CollectedAt: 1700000002000,
						UnitName:    "hotbed-cmd-1.scope",
						Procs: []*pb.ProcessInfo{
							{Pid: 42, Ppid: 1, State: "S", Comm: "worker", ThreadsCount: 2, RssBytes: 4 << 20, Wchan: "futex_wait"},
						},
						Kstacks: []*pb.ThreadKernelStack{
							{Pid: 42, Tid: 42, Comm: "worker", State: "S", Wchan: "futex_wait", Frames: []string{"futex_wait_queue+0x60"}},
						},
						Files: []*pb.OpenFile{
							{Pid: 42, Fd: 3, Kind: "socket", Mode: "rw", Target: "socket:[123]", SocketProto: "tcp", SocketState: "ESTABLISHED", LocalAddr: "127.0.0.1:4000", RemoteAddr: "127.0.0.1:5432"},
						},
						CgroupStats: &pb.CgroupStats{
							CpuUsageUsec:       50000,
							MemoryCurrentBytes: 8 << 20,
							PidsCurrent:        1,
							PidsPeak:           2,
						},
						Maps: []*pb.ProcessMemoryMap{
							{Pid: 42, Comm: "worker", RssBytes: 4 << 20, PssBytes: 3 << 20, TopRegions: []*pb.MemoryRegion{{Perms: "rw-p", SizeBytes: 4 << 20, RssBytes: 4 << 20, PssBytes: 3 << 20, Path: "[heap]"}}},
						},
						Limits: []*pb.ProcessLimits{
							{Pid: 42, Comm: "worker", Seccomp: "0", CapEff: "0000000000000000", Items: []*pb.ResourceLimit{{Name: "Max open files", Soft: "1024", Hard: "65536", Units: "files"}}},
						},
					},
				},
			},
		}

		var plain bytes.Buffer
		if err := writeOutput(&plain, "diagnostics", nil, resp, outputOpts{}); err != nil {
			t.Fatal(err)
		}
		got := plain.String()
		for _, want := range []string{
			"== 1: pause by session deft-emu on laptop (2023-11-14T22:13:20Z): check locks",
			"not all taken: perf unavailable",
			"Sampling command cmd-1 (hotbed-cmd-1.scope)",
			"=== Process 42: worker [S] ===",
			"=== Inspection (hotbed-cmd-1.scope) ===",
			"Processes (1):",
			"Kernel Stacks (1):",
			"futex_wait_queue+0x60",
			"Open Files (1):",
			"Cgroup Stats:",
			"Memory Maps (1):",
			"Limits (1):",
			"(more diagnostics available: hotbed diagnostics cmd-1 --after-seq 1)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("diagnostics plain output missing %q:\n%s", want, got)
			}
		}

		var jsonOut bytes.Buffer
		if err := writeOutput(&jsonOut, "diagnostics", nil, resp, outputOpts{JSON: true}); err != nil {
			t.Fatal(err)
		}
		var env output
		if err := json.Unmarshal(jsonOut.Bytes(), &env); err != nil || !env.OK || env.Command != "diagnostics" {
			t.Fatalf("diagnostics --json envelope invalid: err=%v env=%+v", err, env)
		}
		if !strings.Contains(jsonOut.String(), `"hasMore": true`) || !strings.Contains(jsonOut.String(), `"nextAfterSeq": 1`) {
			t.Errorf("diagnostics --json missing pagination fields:\n%s", jsonOut.String())
		}

		var jqOut bytes.Buffer
		if err := writeOutput(&jqOut, "diagnostics", nil, resp, outputOpts{JQ: ".diagnostics[0].intervention.comment"}); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(jqOut.String()) != "check locks" {
			t.Errorf("diagnostics --jq = %q, want check locks", jqOut.String())
		}
	})
}
