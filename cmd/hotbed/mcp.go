package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	mcpskills "github.com/modelcontextprotocol/go-sdk/extensions/skills"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/skillspec"
	"github.com/semistrict/hotbed/internal/version"
	"github.com/semistrict/hotbed/skills"
)

// hotbed mcp is an MCP server on stdin and stdout, whose tools are
// hotbed.proto's commands and whose skills are served over the MCP Skills
// Extension (SEP-2640, io.modelcontextprotocol/skills) and MCP Resources.
// The server is a session: the agent that starts it works in the session its
// environment names (HOTBED_SESSION, or the one bound to its T3 thread or git
// worktree), resumed if it ended, or in a new one bound there; the session
// lives while the server runs and ends when it exits, and the next server
// started there takes it up again.

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
	skills   *pb.SkillBriefing  // active skill descriptors from the latest StartSession
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
	instructions := "hotbed's tools: builds, tests and dev servers on a hotbed cluster, questions for the user, cross-agent memories, and agent skills (MCP Skills Extension `io.modelcontextprotocol/skills` + `resources/read`). Read `hotbed skills get hotbed` (or `skill://builtin/hotbed/SKILL.md`) for how hotbed works. A reusable lesson goes where it will be found: its fix, the repo's AGENTS.md, a comment by the code, or a skill; save a memory (`memory_add`) only for what has none of those (a preference, a machine fact, a workaround naming its open bug), updating one that covers it (`memory_ls`)."
	var skillBriefing *pb.SkillBriefing
	if id, err := s.ensure(ctx); err != nil {
		fmt.Fprintf(s.d.stderr, "hotbed mcp: no session yet: %v\n", toAPIError(err))
	} else {
		instructions += fmt.Sprintf(" The tools act as session %s, which hotbed commands run here use too.", id)
		s.mu.Lock()
		reminder := s.reminder
		memories := s.memories
		skillBriefing = s.skills
		s.mu.Unlock()
		if memText := formatMemoryBriefingForInstructions(memories); memText != "" {
			instructions += "\n\n" + memText
		}
		if skillText := formatSkillBriefingForInstructions(skillBriefing); skillText != "" {
			instructions += "\n\n" + skillText
		}
		if reminder != "" {
			instructions += "\n\n" + reminder
		}
	}
	defer s.end()

	caps := &mcp.ServerCapabilities{
		Tools:     &mcp.ToolCapabilities{},
		Resources: &mcp.ResourceCapabilities{},
	}
	caps.AddExtension(mcpskills.ExtensionID, map[string]any{"directoryRead": true})

	server := mcp.NewServer(
		&mcp.Implementation{Name: "hotbed", Version: version.String()},
		&mcp.ServerOptions{
			Instructions: instructions,
			Capabilities: caps,
		},
	)
	for _, tool := range schema.Tools() {
		destructive := tool.Destructive
		server.AddTool(&mcp.Tool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.ReadOnly, DestructiveHint: &destructive},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, tool.Command, req.Params.Arguments), nil
		})
	}

	if err := s.registerSkills(server, skillBriefing); err != nil {
		return err
	}

	return server.Run(ctx, t)
}

// registerSkills wires SEP-2640 (`skills/list`, `skills/get`,
// `resources/directory/read`) and standard MCP Resources (`resources/read` and
// `resources/list`) for both controller-stored skills and the compiled-in
// `hotbed` guide.
func (s *mcpServer) registerSkills(server *mcp.Server, initial *pb.SkillBriefing) error {
	// Register a resource template so any skill file (including newly added skills
	// and nested supporting files) is readable via standard `resources/read`.
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "skill://{scope}/{name}/{+path}",
		Name:        "Hotbed Skill File",
		Description: "Files belonging to an Agent Skill served by Hotbed.",
	}, s.readSkillResource)

	// Register top-level SKILL.md resources for standard MCP clients that call
	// `resources/list` without implementing SEP-2640.
	for _, b := range builtinMCPSkills() {
		uri := b.entry.URI
		server.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        b.entry.Frontmatter.Name(),
			Description: b.entry.Frontmatter.Description(),
			MIMEType:    "text/markdown",
		}, s.readSkillResource)
	}
	if initial != nil {
		for _, d := range initial.Skills {
			uri := d.Uri
			server.AddResource(&mcp.Resource{
				URI:         uri,
				Name:        d.Name,
				Description: d.Description,
				MIMEType:    "text/markdown",
			}, s.readSkillResource)
		}
	}

	return mcpskills.AddHandlers(server, &mcpskills.Handlers{
		List:          s.handleListSkills,
		Get:           s.handleGetSkill,
		ReadDirectory: s.handleReadSkillDirectory,
	}, &mcpskills.ServerOptions{
		Limits: mcpskills.BaselineLimits(),
	})
}

type builtinSkillEntry struct {
	name  string
	entry *mcpskills.Skill
	files map[string]skillspec.FileEntry
}

func builtinMCPSkills() []builtinSkillEntry {
	list, err := skills.List()
	if err != nil {
		return nil
	}
	var out []builtinSkillEntry
	for _, sk := range list {
		rawFiles, err := skills.ReadAll(sk.Name)
		if err != nil {
			continue
		}
		skillMD, ok := rawFiles["SKILL.md"]
		if !ok {
			continue
		}
		fm, _, err := skillspec.ParseFrontmatter(skillMD)
		if err != nil {
			continue
		}
		rootURI := fmt.Sprintf("skill://builtin/%s", sk.Name)
		skillURI := rootURI + "/SKILL.md"

		paths := make([]string, 0, len(rawFiles))
		for p := range rawFiles {
			paths = append(paths, p)
		}
		slices.Sort(paths)

		fileMap := make(map[string]skillspec.FileEntry, len(paths))
		resources := make([]*mcpskills.Resource, 0, len(paths))
		for _, p := range paths {
			data := rawFiles[p]
			digest := skillspec.SHA256Digest(data)
			fileMap[p] = skillspec.FileEntry{
				Path:     p,
				Content:  data,
				MIMEType: skillspec.InferMIMEType(p, data),
				Digest:   digest,
				Binary:   skillspec.IsBinary(data),
			}
			resources = append(resources, &mcpskills.Resource{
				URI:    rootURI + "/" + p,
				Digest: digest,
				Size:   int64(len(data)),
			})
		}

		out = append(out, builtinSkillEntry{
			name: sk.Name,
			entry: &mcpskills.Skill{
				URI:         skillURI,
				Frontmatter: mcpskills.Frontmatter(maps.Clone(fm)),
				Resources:   mcpskills.StaticResources(resources...),
			},
			files: fileMap,
		})
	}
	return out
}

func pbSkillToMCPSkill(sk *pb.Skill) (*mcpskills.Skill, error) {
	if sk == nil {
		return nil, fmt.Errorf("nil skill")
	}
	var fm mcpskills.Frontmatter
	if sk.FrontmatterJson != "" {
		if err := json.Unmarshal([]byte(sk.FrontmatterJson), &fm); err != nil {
			return nil, fmt.Errorf("decode frontmatter for %s: %w", sk.Name, err)
		}
	}
	if fm == nil {
		fm = mcpskills.Frontmatter{
			"name":        sk.Name,
			"description": sk.Description,
		}
	}
	rootURI := strings.TrimSuffix(sk.Uri, "/SKILL.md")
	resources := make([]*mcpskills.Resource, 0, len(sk.Files))
	for _, f := range sk.Files {
		resources = append(resources, &mcpskills.Resource{
			URI:    rootURI + "/" + f.Path,
			Digest: f.Digest,
			Size:   f.Size,
		})
	}
	return &mcpskills.Skill{
		URI:         sk.Uri,
		Frontmatter: fm,
		Resources:   mcpskills.StaticResources(resources...),
	}, nil
}

func parseSkillResourceURI(rawURI string) (scope, name, relPath string, ok bool) {
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme != "skill" || u.Host == "" {
		return "", "", "", false
	}
	scope = u.Host
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" {
		return "", "", "", false
	}
	idx := strings.IndexByte(p, '/')
	if idx < 0 {
		return scope, p, "", true
	}
	return scope, p[:idx], p[idx+1:], true
}

func (s *mcpServer) handleListSkills(ctx context.Context, req *mcpskills.ListSkillsRequest) (*mcpskills.ListSkillsResult, error) {
	var catalog []*mcpskills.Skill
	if sessID, err := s.ensure(ctx); err == nil {
		if cl, cerr := s.clients(s.d.http); cerr == nil {
			res, lerr := cl.skills.ListSkills(ctx, &pb.ListSkillsRequest{SessionId: sessID})
			if lerr != nil {
				return nil, lerr
			}
			for _, sk := range res.Skills {
				entry, perr := pbSkillToMCPSkill(sk)
				if perr != nil {
					continue
				}
				catalog = append(catalog, entry)
			}
		}
	}
	for _, b := range builtinMCPSkills() {
		catalog = append(catalog, b.entry)
	}
	page, next, err := paginateMCPSkills(catalog, req.Params.Cursor)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
	}
	return &mcpskills.ListSkillsResult{
		Cacheable:  mcp.Cacheable{TTLMs: 30_000, CacheScope: "private"},
		Skills:     page,
		NextCursor: next,
	}, nil
}

func paginateMCPSkills(all []*mcpskills.Skill, cursor string) ([]*mcpskills.Skill, string, error) {
	const pageSize = 100
	start := 0
	if cursor != "" {
		var n int
		if _, err := fmt.Sscanf(cursor, "offset:%d", &n); err != nil || n < 0 || n > len(all) {
			return nil, "", fmt.Errorf("invalid cursor %q", cursor)
		}
		start = n
	}
	end := min(start+pageSize, len(all))
	var next string
	if end < len(all) {
		next = fmt.Sprintf("offset:%d", end)
	}
	return all[start:end], next, nil
}

func (s *mcpServer) handleGetSkill(ctx context.Context, req *mcpskills.GetSkillRequest) (*mcpskills.GetSkillResult, error) {
	uri := req.Params.URI
	scope, name, rel, ok := parseSkillResourceURI(uri)
	if !ok || rel != "SKILL.md" {
		return nil, nil
	}
	if scope == "builtin" {
		for _, b := range builtinMCPSkills() {
			if b.name == name {
				return &mcpskills.GetSkillResult{
					Cacheable: mcp.Cacheable{TTLMs: 300_000, CacheScope: "public"},
					Skill:     b.entry,
				}, nil
			}
		}
		return nil, nil
	}
	cl, err := s.clients(s.d.http)
	if err != nil {
		return nil, err
	}
	sessID, _ := s.ensure(ctx)
	sk, err := cl.skills.ShowSkill(ctx, &pb.ShowSkillRequest{Ref: uri, SessionId: sessID})
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	entry, err := pbSkillToMCPSkill(sk)
	if err != nil {
		return nil, err
	}
	return &mcpskills.GetSkillResult{
		Cacheable: mcp.Cacheable{TTLMs: 30_000, CacheScope: "private"},
		Skill:     entry,
	}, nil
}

func (s *mcpServer) handleReadSkillDirectory(ctx context.Context, req *mcpskills.ReadDirectoryRequest) (*mcpskills.ReadDirectoryResult, error) {
	dirURI := req.Params.URI
	scope, name, subDir, ok := parseSkillResourceURI(dirURI)
	if !ok {
		return nil, nil
	}
	rootURI := fmt.Sprintf("skill://%s/%s", scope, name)
	type fileInfo struct {
		path     string
		mimeType string
		size     int64
	}
	var allFiles []fileInfo

	if scope == "builtin" {
		var found *builtinSkillEntry
		for _, b := range builtinMCPSkills() {
			if b.name == name {
				cp := b
				found = &cp
				break
			}
		}
		if found == nil {
			return nil, nil
		}
		for _, f := range found.files {
			allFiles = append(allFiles, fileInfo{path: f.Path, mimeType: f.MIMEType, size: int64(len(f.Content))})
		}
	} else {
		cl, err := s.clients(s.d.http)
		if err != nil {
			return nil, err
		}
		sessID, _ := s.ensure(ctx)
		sk, err := cl.skills.ShowSkill(ctx, &pb.ShowSkillRequest{Ref: rootURI + "/SKILL.md", SessionId: sessID})
		if err != nil {
			if api.CodeOf(err) == api.CodeNotFound {
				return nil, nil
			}
			return nil, err
		}
		for _, f := range sk.Files {
			allFiles = append(allFiles, fileInfo{path: f.Path, mimeType: f.MimeType, size: f.Size})
		}
	}

	prefix := ""
	if subDir != "" {
		prefix = subDir + "/"
	}
	childrenMap := map[string]*mcp.Resource{}
	for _, f := range allFiles {
		if subDir != "" && f.path == subDir {
			// Target URI is a file, not a directory.
			return nil, nil
		}
		if !strings.HasPrefix(f.path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f.path, prefix)
		if rest == "" {
			continue
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			dirName := rest[:slash]
			childURI := dirURI + "/" + dirName
			if _, exists := childrenMap[childURI]; !exists {
				childrenMap[childURI] = &mcp.Resource{
					URI:      childURI,
					Name:     dirName,
					MIMEType: "inode/directory",
				}
			}
		} else {
			childURI := dirURI + "/" + rest
			childrenMap[childURI] = &mcp.Resource{
				URI:      childURI,
				Name:     rest,
				MIMEType: f.mimeType,
				Size:     f.size,
			}
		}
	}
	if len(childrenMap) == 0 {
		// Directory does not exist under this skill.
		return nil, nil
	}
	children := make([]*mcp.Resource, 0, len(childrenMap))
	for _, r := range childrenMap {
		children = append(children, r)
	}
	slices.SortFunc(children, func(a, b *mcp.Resource) int {
		return strings.Compare(a.URI, b.URI)
	})
	page, next, err := mcpskills.PaginateDirectoryResources(children, req.Params.Cursor, 0)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
	}
	return &mcpskills.ReadDirectoryResult{
		Cacheable:  mcp.Cacheable{TTLMs: 30_000, CacheScope: "private"},
		Resources:  page,
		NextCursor: next,
	}, nil
}

func (s *mcpServer) readSkillResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	scope, name, rel, ok := parseSkillResourceURI(uri)
	if !ok || rel == "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown skill resource %q", uri)}
	}
	if scope == "builtin" {
		for _, b := range builtinMCPSkills() {
			if b.name == name {
				f, exists := b.files[rel]
				if !exists {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown skill resource %q", uri)}
				}
				return skillFileToResourceResult(uri, f.MIMEType, f.Content, f.Binary), nil
			}
		}
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown skill resource %q", uri)}
	}

	cl, err := s.clients(s.d.http)
	if err != nil {
		return nil, err
	}
	sessID, _ := s.ensure(ctx)
	skillURI := fmt.Sprintf("skill://%s/%s/SKILL.md", scope, name)
	res, err := cl.skills.GetSkill(ctx, &pb.GetSkillRequest{
		Ref:       skillURI,
		Path:      rel,
		SessionId: sessID,
	})
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound || api.CodeOf(err) == api.CodeInvalidArgs {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown skill resource %q", uri)}
		}
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown skill resource %q", uri)}
	}
	f := res.Files[0]
	bin := skillspec.IsBinary(f.Content)
	mimeType := f.MimeType
	if mimeType == "" {
		mimeType = skillspec.InferMIMEType(f.Path, f.Content)
	}
	return skillFileToResourceResult(uri, mimeType, f.Content, bin), nil
}

func skillFileToResourceResult(uri, mimeType string, content []byte, binary bool) *mcp.ReadResourceResult {
	rc := &mcp.ResourceContents{
		URI:      uri,
		MIMEType: mimeType,
	}
	if binary {
		rc.Blob = content
	} else {
		rc.Text = string(content)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{rc}}
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
	return s.runAPI(ctx, s.d, mcpSessions{s}, cmd, args)
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
	named, found, err := sessionFlag{Session: s.d.getenv("HOTBED_SESSION")}.lookupBinding(ctx, s.d)
	if err != nil {
		return "", err
	}
	req := &pb.StartSessionRequest{
		Agent:          s.d.getenv("HOTBED_AGENT"),
		Project:        s.project,
		T3CodeThreadId: s.d.t3codeThreadID(),
	}
	if found {
		req.Id, req.Resume = named.ID, true
	}
	if req.Remote, err = s.d.checkoutRemote(ctx); err != nil {
		return "", err
	}
	req.T3CodeThreadUrl = s.d.t3codeThreadURL(ctx, req.T3CodeThreadId)
	sess, err := cl.sessions.StartSession(ctx, req)
	if err != nil {
		return "", err
	}
	if place, binds := s.d.startPlace(ctx); !found && binds {
		if err := updateSessionMap(sessionsFilePath(s.d), func(m sessionMap) { m.bind(place, sess.Id) }); err != nil {
			return "", api.Errorf(api.CodeInternal, "started session %s, but could not bind it to %s: %v", sess.Id, place, err)
		}
	}
	s.session = sess.Id
	s.reminder = sess.GetReminder()
	s.memories = sess.GetMemories()
	s.skills = sess.GetSkills()
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

func formatSkillBriefingForInstructions(b *pb.SkillBriefing) string {
	if b == nil || len(b.Skills) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skills (%d — load via MCP `skills/get` + `resources/read` or `hotbed skills get NAME`):", len(b.Skills))
	for _, e := range b.Skills {
		scope := e.Project
		if scope == "" {
			scope = "global"
		}
		if e.Repo != "" {
			scope += "/" + e.Repo
		}
		fmt.Fprintf(&sb, "\n  [%s] %s — %s (%s)", scope, e.Name, e.Description, e.Uri)
	}
	return sb.String()
}
