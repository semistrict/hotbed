package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// mcpClient is an agent's MCP client of hotbed mcp, run as the agent runs
// it: a process whose stdin and stdout are the client's.
type mcpClient struct {
	*mcp.ClientSession
	t      *testing.T
	stop   func() int
	stderr *bytes.Buffer
}

// mcp starts hotbed mcp and connects to it.
func (c *cluster) mcp() *mcpClient {
	c.t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	var stderr bytes.Buffer
	d := c.cli
	d.stdin, d.stdout, d.stderr = serverIn, serverOut, &stderr
	done := make(chan int, 1)
	go func() {
		code := run(c.t.Context(), []string{"mcp"}, d)
		serverOut.Close()
		done <- code
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	sess, err := client.Connect(c.t.Context(), &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		c.t.Fatalf("connect to hotbed mcp: %v (stderr %q)", err, stderr.String())
	}
	m := &mcpClient{ClientSession: sess, t: c.t, stderr: &stderr}
	m.stop = func() int {
		sess.Close()
		clientOut.Close()
		return <-done
	}
	return m
}

// call calls a tool and decodes its result into res, failing on an error.
func (m *mcpClient) call(res proto.Message, tool string, args map[string]any) {
	m.t.Helper()
	out, err := m.CallTool(m.t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		m.t.Fatalf("%s: %v", tool, err)
	}
	text := out.Content[0].(*mcp.TextContent).Text
	if out.IsError {
		m.t.Fatalf("%s: %s", tool, text)
	}
	if res != nil {
		if err := protojson.Unmarshal([]byte(text), res); err != nil {
			m.t.Fatal(err)
		}
	}
}

// fails calls a tool and returns the error it gave.
func (m *mcpClient) fails(tool string, args map[string]any) *api.Error {
	m.t.Helper()
	out, err := m.CallTool(m.t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		m.t.Fatalf("%s: %v", tool, err)
	}
	text := out.Content[0].(*mcp.TextContent).Text
	if !out.IsError {
		m.t.Fatalf("%s gave %s, want an error", tool, text)
	}
	e := &api.Error{}
	if err := json.Unmarshal([]byte(text), e); err != nil {
		m.t.Fatalf("%s's error %q: %v", tool, text, err)
	}
	return e
}

var storeQuestionArgs = map[string]any{"questions": []any{map[string]any{
	"question": "Which store should sessions use?", "header": "Store",
	"options": []any{
		map[string]any{"label": "SQLite", "description": "One file"},
		map[string]any{"label": "Postgres", "description": "A server"},
	},
}}}

// The MCP server is a session: it starts one bound where it runs, which
// its tools act as and hotbed commands run there use too; it ends the
// session when it exits, and the next server started there resumes it.
func TestMCPServerIsASession(t *testing.T) {
	c := newCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-mcp")
	m := c.mcp()

	if instr := m.InitializeResult().Instructions; !strings.Contains(instr, "setup steps") || !strings.Contains(instr, "without downloading or compiling anything") {
		t.Errorf("MCP server instructions missing setup-steps reminder: %q", instr)
	}

	tools, err := m.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var add *mcp.Tool
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Name == "question_add" {
			add = tool
		}
	}
	for _, want := range []string{"question_add", "question_ls", "question_outbox", "question_move", "poll"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
	var schema struct{ Properties map[string]any }
	b, err := json.Marshal(add.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	var params []string
	for k := range schema.Properties {
		params = append(params, k)
	}
	slices.Sort(params)
	if !slices.Equal(params, []string{"answered", "questions", "repo"}) {
		t.Errorf("question_add takes %v, want [answered questions repo]: no session, the server is the session", params)
	}

	added := &pb.AddQuestionsResponse{}
	m.call(added, "question_add", storeQuestionArgs)
	session := added.Questions[0].Asked.Session
	if session == "" || added.Questions[0].Project != "test" {
		t.Fatalf("question asked as %q in project %q, want the server's session in the tests' project", session, added.Questions[0].Project)
	}
	if want := c.cockpit().base + "/questions/" + fmt.Sprint(added.Questions[0].Id); added.Questions[0].CockpitUrl != want {
		t.Errorf("question_add's question links to %q, want its page %s", added.Questions[0].CockpitUrl, want)
	}
	var current sessionBinding
	c.ok(&current, "session", "current")
	if current.ID != session || current.Source != "thread" {
		t.Errorf("session current = %+v, want %s bound to the thread", current, session)
	}
	listed := &pb.ListQuestionsResponse{}
	c.ok(listed, "question", "ls")
	if len(listed.Questions) != 1 {
		t.Errorf("hotbed question ls in the server's thread = %v, want its question", listed.Questions)
	}

	if code := m.stop(); code != exitOK {
		t.Fatalf("hotbed mcp exited %d: %s", code, m.stderr)
	}
	ended := &pb.GetSessionResponse{}
	if err := c.getSession(session, ended); err != nil {
		t.Fatal(err)
	}
	if ended.Session.EndedAt == nil {
		t.Fatalf("session %s is live after its server exited", session)
	}

	m = c.mcp()
	defer m.stop()
	shown := &pb.ListQuestionsResponse{}
	m.call(shown, "question_ls", map[string]any{"state": []string{"all"}})
	if len(shown.Questions) != 1 {
		t.Errorf("question_ls = %v", shown.Questions)
	}
	resumed := &pb.GetSessionResponse{}
	if err := c.getSession(session, resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Session.EndedAt != nil {
		t.Errorf("session %s did not resume with the next server started in its thread", session)
	}
}

// HOTBED_SESSION names the server's session: made, or taken up, by that
// name.
func TestMCPServerTakesTheNamedSession(t *testing.T) {
	c := newCluster(t)
	t.Setenv("HOTBED_SESSION", "named-otter")
	m := c.mcp()

	if instr := m.InitializeResult().Instructions; !strings.Contains(instr, "setup steps") || !strings.Contains(instr, "without downloading or compiling anything") {
		t.Errorf("MCP server instructions missing setup-steps reminder: %q", instr)
	}
	defer m.stop()
	added := &pb.AddQuestionsResponse{}
	m.call(added, "question_add", storeQuestionArgs)
	if got := added.Questions[0].Asked.Session; got != "named-otter" {
		t.Errorf("asked as %q, want named-otter", got)
	}
}

// A tool's failure is the tool's error, with hotbed's code, for the agent
// to read; arguments the tool does not take fail before anything is called.
func TestMCPToolErrors(t *testing.T) {
	c := newCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-errors")
	m := c.mcp()

	if instr := m.InitializeResult().Instructions; !strings.Contains(instr, "setup steps") || !strings.Contains(instr, "without downloading or compiling anything") {
		t.Errorf("MCP server instructions missing setup-steps reminder: %q", instr)
	}
	defer m.stop()
	if e := m.fails("question_show", map[string]any{"id": 99}); e.Code != api.CodeNotFound {
		t.Errorf("question_show 99 = %v, want NOT_FOUND", e)
	}
	if e := m.fails("question_ls", map[string]any{"session": "other-owl"}); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "takes no [session]") {
		t.Errorf("question_ls with a session = %v, want INVALID_ARGS: the server is the session", e)
	}
	if e := m.fails("question_ls", map[string]any{"sort": "sideways"}); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, `"sideways" is not one of newest, oldest, updated, rank`) {
		t.Errorf("question_ls sort sideways = %v, want INVALID_ARGS naming the words", e)
	}
	if e := m.fails("question_ls", map[string]any{"project": "test", "all_projects": true}); e.Code != api.CodeInvalidArgs || !strings.Contains(e.Message, "all_projects and project are one or the other") {
		t.Errorf("question_ls naming a project and all of them = %v, want INVALID_ARGS", e)
	}
	m.call(nil, "question_add", storeQuestionArgs)
	moved := &pb.Question{}
	m.call(moved, "question_move", map[string]any{"id": 1, "last": true})
	if moved.Id != 1 {
		t.Errorf("question_move = %v", moved)
	}
}

// TestMCPRegistersEveryProtoCommand verifies that hotbed mcp registers an MCP
// tool for every option (command) RPC declared in hotbed.proto, with matching
// annotations, and exercises newly migrated CRUD tools end to end.
func TestMCPRegistersEveryProtoCommand(t *testing.T) {
	c := newCluster(t)
	t.Setenv("T3CODE_THREAD_ID", "thread-mcp-coverage")
	m := c.mcp()
	defer m.stop()

	schema, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	// Cross-check schema against the compiled proto descriptor directly so no
	// option (command) annotation can silently drop out.
	var wantTools []string
	svcs := pb.File_hotbed_v1_hotbed_proto.Services()
	for i := range svcs.Len() {
		ms := svcs.Get(i).Methods()
		for j := range ms.Len() {
			method := ms.Get(j)
			if method.Options() == nil || !proto.HasExtension(method.Options(), pb.E_Command) {
				continue
			}
			for _, cs := range proto.GetExtension(method.Options(), pb.E_Command).([]*pb.CommandSpec) {
				wantTools = append(wantTools, strings.Join(strings.Fields(cs.GetName()), "_"))
			}
		}
	}
	slices.Sort(wantTools)

	listed, err := m.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	gotByName := map[string]*mcp.Tool{}
	var gotTools []string
	for _, tool := range listed.Tools {
		gotTools = append(gotTools, tool.Name)
		gotByName[tool.Name] = tool
	}
	slices.Sort(gotTools)
	if !slices.Equal(gotTools, wantTools) {
		t.Fatalf("MCP tools = %v\nwant %v", gotTools, wantTools)
	}
	for _, cmd := range schema.Commands {
		tool := gotByName[cmd.Tool()]
		if tool == nil {
			t.Errorf("missing MCP tool %q for command %q", cmd.Tool(), cmd.Name())
			continue
		}
		if tool.Description == "" {
			t.Errorf("MCP tool %q has empty description", tool.Name)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != cmd.ReadOnly {
			t.Errorf("MCP tool %q ReadOnlyHint = %+v, want %v", tool.Name, tool.Annotations, cmd.ReadOnly)
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != cmd.Destructive {
			t.Errorf("MCP tool %q DestructiveHint = %+v, want %v", tool.Name, tool.Annotations, cmd.Destructive)
		}
	}

	// Exercise migrated CRUD tools over MCP end to end.
	nodes := &pb.ListNodesResponse{}
	m.call(nodes, "nodes", nil)
	if len(nodes.Nodes) != 1 {
		t.Errorf("nodes = %v, want 1 worker", nodes.Nodes)
	}

	proj := &pb.Project{}
	m.call(proj, "project_add", map[string]any{"name": "mcp-proj", "repo": []string{"https://github.com/o/mcp-app.git"}})
	if proj.Name != "mcp-proj" || !slices.Equal(proj.Repos, []string{"github.com/o/mcp-app"}) {
		t.Errorf("project_add = %v", proj)
	}
	shown := &pb.Project{}
	m.call(shown, "project_show", map[string]any{"name": "mcp-proj"})
	if shown.Name != "mcp-proj" {
		t.Errorf("project_show = %v", shown)
	}
	projs := &pb.ListProjectsResponse{}
	m.call(projs, "project_ls", nil)
	if !slices.ContainsFunc(projs.Projects, func(p *pb.Project) bool { return p.Name == "mcp-proj" }) {
		t.Errorf("project_ls missing mcp-proj: %v", projs.Projects)
	}

	repo := &pb.Repo{}
	m.call(repo, "repo_add", map[string]any{"id": "mcp-app", "url": "https://github.com/o/mcp-app.git", "project": "mcp-proj"})
	if repo.Id != "mcp-app" {
		t.Errorf("repo_add = %v", repo)
	}
	repos := &pb.ListReposResponse{}
	m.call(repos, "repo_ls", map[string]any{"project": "mcp-proj"})
	if len(repos.Repos) != 1 || repos.Repos[0].Id != "mcp-app" {
		t.Errorf("repo_ls = %v", repos.Repos)
	}
	m.call(nil, "repo_rm", map[string]any{"id": "mcp-app"})

	ps := &pb.ListCommandsResponse{}
	m.call(ps, "ps", map[string]any{"all": true, "limit": 0})
	snap := &pb.Snapshot{}
	m.call(snap, "snapshot", nil)
	if snap.ClusterId == "" {
		t.Errorf("snapshot returned empty cluster_id")
	}
}

// getSession is the session as the controller has it.
func (c *cluster) getSession(id string, res *pb.GetSessionResponse) error {
	addr, err := c.cli.controller()
	if err != nil {
		return err
	}
	cl, err := controllerClient{Controller: addr}.clients(c.cli.http)
	if err != nil {
		return err
	}
	got, err := cl.sessions.GetSession(c.t.Context(), &pb.GetSessionRequest{Id: id})
	if err != nil {
		return err
	}
	proto.Merge(res, got)
	return nil
}
