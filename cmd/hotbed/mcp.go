package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/version"
)

// hotbed mcp is an MCP server on stdin and stdout, whose tools are
// hotbed.proto's commands. The server is a session: the agent that starts
// it works in the session its environment names (HOTBED_SESSION, or the
// one bound to its T3 thread or git worktree), resumed if it ended, or in
// a new one bound there; the session lives while the server runs and ends
// when it exits, and the next server started there takes it up again.

type mcpCmd struct {
	controllerClient
	projectFlag
}

func (c *mcpCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	srv := newMCPServer(c.controllerClient, d, c.Project)
	if err := srv.serve(ctx, &mcp.IOTransport{Reader: io.NopCloser(d.stdin), Writer: nopWriteCloser{d.stdout}}); err != nil {
		return exitError, true, err
	}
	return exitOK, true, nil
}

func (c *mcpCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("mcp is raw") }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// mcpServer is hotbed's MCP server and the session it is.
type mcpServer struct {
	controllerClient
	d       deps
	project string // a new session's project; empty for the checkout's

	// alive is how long the session is kept alive: while the server serves.
	alive context.Context

	mu       sync.Mutex
	session  string             // empty until started, and again once it ends
	reminder string             // setup reminder from the latest StartSession
	memories *pb.MemoryBriefing // compact memory briefing from the latest StartSession
	// kept counts the sessions started: a keep-alive goes on only while
	// its session is the latest, so a session resumed is kept by one.
	kept   int
	closed bool // the server has ended its session, and takes no other
}

func newMCPServer(c controllerClient, d deps, project string) *mcpServer {
	return &mcpServer{controllerClient: c, d: d, project: project}
}

// serve serves t until its client goes away or ctx ends, as a session it
// starts first and ends last.
func (s *mcpServer) serve(ctx context.Context, t mcp.Transport) error {
	schema, err := apiSchema()
	if err != nil {
		return err
	}
	alive, stopKeeping := context.WithCancel(ctx)
	defer stopKeeping()
	s.alive = alive
	// Started now, before the client asks for anything; if the controller
	// cannot be reached, each tool call tries again.
	instructions := "hotbed's tools: builds, tests and dev servers on a hotbed cluster, questions for the user, and cross-agent memories. Read `hotbed skills get hotbed` for how hotbed works. Save reusable lessons as memories (`memory_add`) whenever you hit a repo/tooling quirk, resolve a tricky bug, or the user corrects you."
	if id, err := s.ensure(ctx); err != nil {
		fmt.Fprintf(s.d.stderr, "hotbed mcp: no session yet: %v\n", toAPIError(err))
	} else {
		instructions += fmt.Sprintf(" The tools act as session %s, which hotbed commands run here use too.", id)
		s.mu.Lock()
		reminder := s.reminder
		memories := s.memories
		s.mu.Unlock()
		if memText := formatMemoryBriefingForInstructions(memories); memText != "" {
			instructions += "\n\n" + memText
		}
		if reminder != "" {
			instructions += "\n\n" + reminder
		}
	}
	defer s.end()

	server := mcp.NewServer(&mcp.Implementation{Name: "hotbed", Version: version.String()}, &mcp.ServerOptions{Instructions: instructions})
	for _, tool := range schema.Tools() {
		destructive := tool.Destructive
		server.AddTool(&mcp.Tool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.ReadOnly, DestructiveHint: &destructive},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, tool.Command, req.Params.Arguments), nil
		})
	}
	return server.Run(ctx, t)
}

// call runs a tool's command; what fails is the tool's error, for the
// agent to read.
func (s *mcpServer) call(ctx context.Context, cmd *apicmd.Command, arguments json.RawMessage) *mcp.CallToolResult {
	res, err := s.run(ctx, cmd, arguments)
	if err != nil {
		b, merr := json.Marshal(toAPIError(err))
		if merr != nil {
			b = []byte(err.Error())
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
	}
	s.d.cockpit.fill(res)
	b, err := protojson.MarshalOptions{EmitDefaultValues: true, Indent: "  "}.Marshal(res)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: json.RawMessage(b)}
}

func (s *mcpServer) run(ctx context.Context, cmd *apicmd.Command, arguments json.RawMessage) (proto.Message, error) {
	args, err := cmd.MCPArgs(arguments)
	if err != nil {
		return nil, err
	}
	r, err := s.runner(s.d, mcpSessions{s})
	if err != nil {
		return nil, err
	}
	return r.Run(ctx, cmd, args)
}

// mcpSessions is the server's session, whatever a tool was given.
type mcpSessions struct{ s *mcpServer }

func (m mcpSessions) Session(ctx context.Context, _ string) (string, error) { return m.s.ensure(ctx) }

// Named is the server's session too: an agent's change is its session's.
func (m mcpSessions) Named(ctx context.Context, _ string) (string, error) { return m.s.ensure(ctx) }

// ensure is the server's session, started (or resumed) if it has none.
func (s *mcpServer) ensure(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != "" {
		return s.session, nil
	}
	if s.closed {
		return "", api.Errorf(api.CodeConflict, "hotbed mcp is exiting: its session has ended")
	}
	cl, err := s.clients(s.d.http)
	if err != nil {
		return "", err
	}
	// The session the environment names: HOTBED_SESSION, or the one bound
	// to this T3 thread or git worktree.
	named, found, err := sessionFlag{Session: os.Getenv("HOTBED_SESSION")}.lookupBinding(ctx, s.d)
	if err != nil {
		return "", err
	}
	req := &pb.StartSessionRequest{
		Agent:          os.Getenv("HOTBED_AGENT"),
		Project:        s.project,
		T3CodeThreadId: strings.TrimSpace(os.Getenv("T3CODE_THREAD_ID")),
	}
	if found {
		req.Id, req.Resume = named.ID, true
	}
	if req.Remote, err = checkoutRemote(ctx); err != nil {
		return "", err
	}
	sess, err := cl.sessions.StartSession(ctx, req)
	if err != nil {
		return "", err
	}
	if place, binds := startPlace(ctx); !found && binds {
		if err := updateSessionMap(sessionsFilePath(s.d), func(m sessionMap) { m.bind(place, sess.Id) }); err != nil {
			return "", api.Errorf(api.CodeInternal, "started session %s, but could not bind it to %s: %v", sess.Id, place, err)
		}
	}
	s.session = sess.Id
	s.reminder = sess.GetReminder()
	s.memories = sess.GetMemories()
	s.kept++
	go s.keepAlive(s.alive, sess.Id, s.kept)
	return s.session, nil
}

// keepAlive watches session id until it ends, or the server does, which
// keeps it from being abandoned. A session that ends meanwhile is resumed
// by the next tool call.
func (s *mcpServer) keepAlive(ctx context.Context, id string, kept int) {
	var after int64
	for ctx.Err() == nil && s.keeps(id, kept) {
		if err := s.watch(ctx, id, &after); err != nil && ctx.Err() == nil {
			slog.Warn("hotbed mcp: keep the session alive", "session", id, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}
}

// keeps says whether keep-alive number kept still keeps session id.
func (s *mcpServer) keeps(id string, kept int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session == id && s.kept == kept
}

// watch waits for the session to change, and forgets it if it ended.
func (s *mcpServer) watch(ctx context.Context, id string, after *int64) error {
	stream, err := s.clients(s.d.stream)
	if err != nil {
		return err
	}
	res, err := watchSession(ctx, stream, &pb.WatchSessionRequest{SessionId: id, After: *after, TimeoutSeconds: 120})
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound {
			s.forget(id)
		}
		return err
	}
	*after = res.Cursor
	if res.Ended {
		s.forget(id)
	}
	return nil
}

func (s *mcpServer) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == id {
		s.session = ""
	}
}

// end ends the server's session, keeping its binding: the next server
// started here resumes it.
func (s *mcpServer) end() {
	s.mu.Lock()
	id := s.session
	s.session, s.closed = "", true
	s.mu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, err := s.clients(s.d.http)
	if err == nil {
		_, err = cl.sessions.EndSession(ctx, &pb.EndSessionRequest{Id: id})
	}
	if err != nil && api.CodeOf(err) != api.CodeNotFound {
		fmt.Fprintf(s.d.stderr, "hotbed mcp: could not end session %s: %v\n", id, strings.TrimSpace(toAPIError(err).Error()))
	}
}

func formatMemoryBriefingForInstructions(b *pb.MemoryBriefing) string {
	if b == nil || (len(b.Always) == 0 && len(b.OnDemand) == 0) {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Memories (%d always-on, %d on-demand — fetch details with tool `memory_get` or `hotbed memory get SLUG`):", len(b.Always), len(b.OnDemand))
	scopeTag := func(e *pb.MemoryIndexEntry) string {
		s := e.Project
		if s == "" {
			s = "global"
		}
		if e.Repo != "" {
			s += "/" + e.Repo
		}
		return s
	}
	if len(b.Always) > 0 {
		sb.WriteString("\nAlways-on:")
		for _, e := range b.Always {
			fmt.Fprintf(&sb, "\n  [%s] %s: %s", scopeTag(e), e.Slug, e.Summary)
		}
	}
	if b.OmittedAlways > 0 {
		fmt.Fprintf(&sb, "\n  (+%d more always-on memories — call `memory_ls` with mode=always)", b.OmittedAlways)
	}
	if len(b.OnDemand) > 0 {
		sb.WriteString("\nOn-demand (read when relevant):")
		for _, e := range b.OnDemand {
			fmt.Fprintf(&sb, "\n  [%s] %s — %s", scopeTag(e), e.Slug, e.Summary)
		}
	}
	if b.OmittedOnDemand > 0 {
		fmt.Fprintf(&sb, "\n  (+%d more on-demand memories — call `memory_ls`)", b.OmittedOnDemand)
	}
	return sb.String()
}
