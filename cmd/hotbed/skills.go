package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/skillspec"
	"github.com/semistrict/hotbed/skills"
	"google.golang.org/protobuf/encoding/protojson"
)

func marshalSkillsProtoJSON(list []*pb.Skill) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(list))
	opts := protojson.MarshalOptions{EmitDefaultValues: true}
	for _, s := range list {
		b, err := opts.Marshal(s)
		if err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, nil
}

func marshalSkillProtoJSON(s *pb.Skill) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	b, err := protojson.MarshalOptions{EmitDefaultValues: true}.Marshal(s)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func unmarshalSkillsProtoJSON(raw []json.RawMessage) ([]*pb.Skill, error) {
	if raw == nil {
		return nil, nil
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	out := make([]*pb.Skill, 0, len(raw))
	for _, b := range raw {
		s := &pb.Skill{}
		if err := opts.Unmarshal(b, s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func unmarshalSkillProtoJSON(raw json.RawMessage) (*pb.Skill, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	s := &pb.Skill{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, s); err != nil {
		return nil, err
	}
	return s, nil
}

type skillsCmd struct {
	Ls      skillsLsCmd      `cmd:"" aliases:"list" help:"List skills stored in hotbed (global + active project) and built-in guides."`
	Get     skillsGetCmd     `cmd:"" help:"Print a skill's SKILL.md or a supporting file (by name, scope/name, ID, or skill:// URI)."`
	Show    skillsShowCmd    `cmd:"" help:"Show a skill's full metadata, file manifest (with SHA-256 digests), source provenance, and history."`
	Add     skillsAddCmd     `cmd:"" help:"Install skill(s) into hotbed from any npx skills source (owner/repo, GitHub/GitLab/Git URL, local dir, well-known URL, or archive)."`
	Install skillsInstallCmd `cmd:"" aliases:"i" help:"Install built-in guides (default), a source, or hotbed skills (--from-hotbed) into local coding agent directories."`
	Find    skillsFindCmd    `cmd:"" help:"Search for skills on skills.sh."`
	Check   skillsCheckCmd   `cmd:"" help:"Check hotbed skills installed from upstream sources for available updates."`
	Update  skillsUpdateCmd  `cmd:"" help:"Update hotbed skills from their recorded upstream sources."`
	Init    skillsInitCmd    `cmd:"" help:"Scaffold a new SKILL.md template locally."`
	Edit    skillsEditCmd    `cmd:"" help:"Edit a skill's scope, project, repo, tags, or rationale."`
	Promote skillsPromoteCmd `cmd:"" help:"Promote a project-scoped skill to a global skill shared across all projects."`
	Archive skillsArchiveCmd `cmd:"" help:"Archive an active skill so it is excluded from session start and MCP (restorable)."`
	Restore skillsRestoreCmd `cmd:"" help:"Restore an archived skill back to active."`
	Rm      skillsRmCmd      `cmd:"" aliases:"remove" help:"Permanently remove a skill, its files, and its history."`
	Move    skillsMoveCmd    `cmd:"" help:"Move an active skill in startup priority order."`
}

// tryControllerClients connects to the configured controller when available.
// Returns connected=false (nil error) only when no controller is configured on
// this machine, so offline commands like `skills get hotbed` and `skills install`
// work without a cluster.
func tryControllerClients(d deps) (clients, cockpit, bool, error) {
	if d.controller == nil {
		return clients{}, cockpit{}, false, nil
	}
	addr, err := d.controller()
	if err != nil {
		if api.CodeOf(err) == api.CodeNotInitialized {
			return clients{}, cockpit{}, false, nil
		}
		return clients{}, cockpit{}, false, err
	}
	cc := controllerClient{}
	cc.callController(addr, d)
	cl, err := cc.clients(d.http)
	if err != nil {
		return clients{}, cockpit{}, false, err
	}
	cp, err := newCockpit(addr)
	if err != nil {
		return clients{}, cockpit{}, false, err
	}
	return cl, cp, true, nil
}

// ---- ls / list ----

type skillsLsCmd struct {
	sessionFlag
	projectFlag
	Scope       string   `enum:",global,project" default:"" help:"Filter by scope: global or project; omit for global + active project."`
	AllProjects bool     `name:"all-projects" help:"List skills across global and every project."`
	State       []string `help:"Filter by state (active, archived, all); default active."`
	Tag         string   `help:"Filter by tag."`
	Query       string   `short:"q" help:"Case-insensitive substring search across name, description, tags, source, and file paths."`
	Sort        string   `enum:",rank,newest,oldest,updated,name" default:"" help:"Sort order: rank, newest, oldest, updated, or name."`
	Limit       int32    `short:"n" help:"Maximum skills to return; 0 for all."`
	Builtin     bool     `help:"Only list guides compiled into the hotbed binary."`
}

type skillsListResult struct {
	Skills  []*pb.Skill    `json:"skills"`
	Total   int32          `json:"total"`
	Builtin []skills.Skill `json:"builtin,omitempty"`
}

func (r skillsListResult) MarshalJSON() ([]byte, error) {
	sk, err := marshalSkillsProtoJSON(r.Skills)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Skills  []json.RawMessage `json:"skills"`
		Total   int32             `json:"total"`
		Builtin []skills.Skill    `json:"builtin,omitempty"`
	}{
		Skills:  sk,
		Total:   r.Total,
		Builtin: r.Builtin,
	})
}

func (r *skillsListResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Skills  []json.RawMessage `json:"skills"`
		Total   int32             `json:"total"`
		Builtin []skills.Skill    `json:"builtin,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	sk, err := unmarshalSkillsProtoJSON(raw.Skills)
	if err != nil {
		return err
	}
	r.Skills = sk
	r.Total = raw.Total
	r.Builtin = raw.Builtin
	return nil
}

func (c *skillsLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	builtins, err := skills.List()
	if err != nil {
		return nil, err
	}
	if c.Builtin {
		return skillsListResult{Skills: []*pb.Skill{}, Builtin: builtins}, nil
	}
	cl, cp, ok, err := tryControllerClients(d)
	if err != nil {
		return nil, err
	}
	if !ok {
		return skillsListResult{Skills: []*pb.Skill{}, Builtin: builtins}, nil
	}

	req := &pb.ListSkillsRequest{
		Tag:   c.Tag,
		Query: c.Query,
		Limit: c.Limit,
	}
	switch c.Scope {
	case "global":
		req.Scope = pb.SkillScope_SKILL_SCOPE_GLOBAL
	case "project":
		req.Scope = pb.SkillScope_SKILL_SCOPE_PROJECT
	}
	switch c.Sort {
	case "rank":
		req.Order = pb.SkillOrder_SKILL_ORDER_RANK
	case "newest":
		req.Order = pb.SkillOrder_SKILL_ORDER_NEWEST
	case "oldest":
		req.Order = pb.SkillOrder_SKILL_ORDER_OLDEST
	case "updated":
		req.Order = pb.SkillOrder_SKILL_ORDER_UPDATED
	case "name":
		req.Order = pb.SkillOrder_SKILL_ORDER_NAME
	}
	for _, st := range c.State {
		switch strings.ToLower(strings.TrimSpace(st)) {
		case "all":
			req.States = []pb.SkillState{pb.SkillState_SKILL_STATE_ACTIVE, pb.SkillState_SKILL_STATE_ARCHIVED}
		case "active":
			req.States = append(req.States, pb.SkillState_SKILL_STATE_ACTIVE)
		case "archived":
			req.States = append(req.States, pb.SkillState_SKILL_STATE_ARCHIVED)
		default:
			return nil, api.Errorf(api.CodeInvalidArgs, "unknown skill state %q (use active, archived, or all)", st)
		}
	}
	if c.Scope != "global" {
		sc, err := c.projectFlag.listing(ctx, d, c.sessionFlag, c.AllProjects)
		if err != nil {
			return nil, err
		}
		if sc.all {
			req.ProjectScope = &pb.ListSkillsRequest_AllProjects{AllProjects: true}
		} else if sc.project != "" {
			req.ProjectScope = &pb.ListSkillsRequest_Project{Project: sc.project}
		}
		req.SessionId = sc.session
		req.Remote = sc.remote
	}

	res, err := cl.skills.ListSkills(ctx, req)
	if err != nil {
		return nil, err
	}
	cp.fill(res)
	out := skillsListResult{Skills: res.Skills, Total: res.Total}
	if out.Skills == nil {
		out.Skills = []*pb.Skill{}
	}
	if c.Scope == "" && c.Tag == "" && len(c.State) == 0 && (c.Query == "" || strings.Contains("hotbed", strings.ToLower(c.Query))) {
		out.Builtin = builtins
	}
	return out, nil
}

// ---- get ----

type skillsGetCmd struct {
	sessionFlag
	projectFlag
	Ref      string `arg:"" help:"Skill name, scope/name, ID, skill:// URI, or built-in guide (e.g. hotbed, hotbed/references/errors.md)."`
	Path     string `arg:"" optional:"" help:"Optional supporting file path inside the skill (default: SKILL.md)."`
	AllFiles bool   `name:"all-files" help:"Return every file in the skill package."`
}

type skillsGetResult struct {
	Name    string           `json:"name"`
	Content string           `json:"content,omitempty"`
	Skill   *pb.Skill        `json:"skill,omitempty"`
	Files   []skillsFileView `json:"files,omitempty"`
}

func (r skillsGetResult) MarshalJSON() ([]byte, error) {
	sk, err := marshalSkillProtoJSON(r.Skill)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Name    string           `json:"name"`
		Content string           `json:"content,omitempty"`
		Skill   json.RawMessage  `json:"skill,omitempty"`
		Files   []skillsFileView `json:"files,omitempty"`
	}{
		Name:    r.Name,
		Content: r.Content,
		Skill:   sk,
		Files:   r.Files,
	})
}

func (r *skillsGetResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name    string           `json:"name"`
		Content string           `json:"content,omitempty"`
		Skill   json.RawMessage  `json:"skill,omitempty"`
		Files   []skillsFileView `json:"files,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	sk, err := unmarshalSkillProtoJSON(raw.Skill)
	if err != nil {
		return err
	}
	r.Name = raw.Name
	r.Content = raw.Content
	r.Skill = sk
	r.Files = raw.Files
	return nil
}

type skillsFileView struct {
	Path     string `json:"path"`
	MIMEType string `json:"mimeType,omitempty"`
	Size     int    `json:"size"`
	Content  string `json:"content,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
}

func (c *skillsGetCmd) Exec(ctx context.Context, d deps) (any, error) {
	// Resolve compiled-in guides offline first when ref matches a built-in (e.g. "hotbed" or "hotbed/references/...").
	builtinTarget := c.Ref
	if c.Path != "" {
		builtinTarget = strings.TrimSuffix(c.Ref, "/") + "/" + strings.TrimPrefix(c.Path, "/")
	}
	if strings.HasPrefix(c.Ref, "skill://builtin/") {
		builtinTarget = strings.TrimPrefix(c.Ref, "skill://builtin/")
		if c.Path != "" {
			builtinTarget = strings.TrimSuffix(strings.TrimSuffix(builtinTarget, "/SKILL.md"), "/") + "/" + strings.TrimPrefix(c.Path, "/")
		}
	}
	if !c.AllFiles {
		if content, err := skills.Get(builtinTarget); err == nil {
			// Prefer a controller skill if one explicitly exists with a non-builtin ref,
			// unless ref is explicitly "hotbed" / "hotbed/..." / "skill://builtin/..." or no controller is set.
			if strings.HasPrefix(c.Ref, "skill://builtin/") || c.Ref == "hotbed" || strings.HasPrefix(c.Ref, "hotbed/") {
				return skillsGetResult{Name: builtinTarget, Content: content}, nil
			}
		}
	}

	cl, cp, ok, err := tryControllerClients(d)
	if err != nil {
		return nil, err
	}
	if !ok {
		if content, berr := skills.Get(builtinTarget); berr == nil {
			return skillsGetResult{Name: builtinTarget, Content: content}, nil
		}
		return nil, api.Errorf(api.CodeInvalidArgs, "no built-in skill %q and no controller configured", c.Ref).
			WithSuggestion("hotbed skills ls lists available skills")
	}

	req := &pb.GetSkillRequest{
		Ref:      c.Ref,
		Path:     c.Path,
		AllFiles: c.AllFiles,
		Project:  c.Project,
	}
	if req.Project == "" {
		if sessID, err := c.sessionFlag.optional(ctx, d); err == nil && sessID != "" {
			req.SessionId = sessID
		} else if remote, err := d.checkoutRemote(ctx); err == nil && remote != "" {
			req.Remote = remote
		}
	}
	res, err := cl.skills.GetSkill(ctx, req)
	if err != nil {
		if api.CodeOf(err) == api.CodeNotFound && !c.AllFiles {
			if content, berr := skills.Get(builtinTarget); berr == nil {
				return skillsGetResult{Name: builtinTarget, Content: content}, nil
			}
		}
		return nil, err
	}
	cp.fill(res)

	out := skillsGetResult{
		Name:  res.Skill.Name,
		Skill: res.Skill,
	}
	for _, f := range res.Files {
		bin := skillspec.IsBinary(f.Content)
		v := skillsFileView{
			Path:     f.Path,
			MIMEType: f.MimeType,
			Size:     len(f.Content),
			Binary:   bin,
		}
		if !bin {
			v.Content = string(f.Content)
		}
		out.Files = append(out.Files, v)
	}
	if len(out.Files) == 1 && !out.Files[0].Binary {
		out.Content = out.Files[0].Content
	} else if len(out.Files) > 1 {
		var sb strings.Builder
		for i, f := range out.Files {
			if i > 0 {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "=== %s (%d B) ===\n", f.Path, f.Size)
			if f.Binary {
				fmt.Fprintf(&sb, "(binary %s)\n", f.MIMEType)
			} else {
				sb.WriteString(f.Content)
				if !strings.HasSuffix(f.Content, "\n") {
					sb.WriteByte('\n')
				}
			}
		}
		out.Content = sb.String()
	}
	return out, nil
}

// ---- show ----

type skillsShowCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref string `arg:"" help:"Skill ID, name, scope/name, or skill:// URI."`
}

func (c *skillsShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	req := &pb.ShowSkillRequest{Ref: c.Ref, Project: c.Project}
	if req.Project == "" {
		if sessID, err := c.sessionFlag.optional(ctx, d); err == nil && sessID != "" {
			req.SessionId = sessID
		} else if remote, err := d.checkoutRemote(ctx); err == nil && remote != "" {
			req.Remote = remote
		}
	}
	return cl.skills.ShowSkill(ctx, req)
}

// ---- add (npx skills-compatible installation into Hotbed, plus optional --local agent install) ----

type skillsAddCmd struct {
	Source    string   `arg:"" help:"Skill source (owner/repo, owner/repo@skill, owner/repo@v1.2.3, GitHub/GitLab/Git URL, local path, well-known URL, or archive URL)."`
	Global    bool     `short:"g" help:"Install as a global skill available across all projects (default: active project)."`
	Project   string   `short:"p" help:"Project to scope installed skills to."`
	Repo      string   `help:"Optional repo within the project."`
	Skill     []string `short:"s" name:"skill" help:"Install specific skill(s) by name (use '*' for all). Repeatable."`
	List      bool     `short:"l" help:"List available skills in the source without installing."`
	Rename    string   `help:"Install the single selected skill under a new name (updates SKILL.md frontmatter name)."`
	All       bool     `help:"Install all discovered skills (with --local, to all agents)."`
	FullDepth bool     `name:"full-depth" help:"Search all subdirectories even when a root SKILL.md exists."`
	Ref       string   `help:"Git branch, tag, or commit SHA to pin."`
	Tag       []string `help:"Optional tag(s) to attach to installed skills. Repeatable."`
	Rationale string   `help:"Optional reason/provenance note for installing this skill."`
	Model     string   `env:"HOTBED_MODEL" help:"Model ID installing this skill (optional)."`
	Session   string   `name:"session" env:"HOTBED_SESSION" help:"Session ID (default: HOTBED_SESSION or bound session)."`
	Yes       bool     `short:"y" help:"Skip confirmation prompts (no-op; hotbed skills add is non-interactive)."`
	Local     bool     `help:"Also (or only, when --no-controller) install into local coding agent directories like npx skills."`
	Agent     []string `short:"a" name:"agent" help:"Target local coding agent(s) (e.g. claude-code, codex, cursor, or '*'). Implies --local."`
	Copy      bool     `help:"Copy files instead of symlinking when installing to local agent directories."`
}

type skillsAddResult struct {
	Source     string                          `json:"source"`
	SourceType string                          `json:"sourceType"`
	Commit     string                          `json:"commit,omitempty"`
	Listed     []skillsPreviewEntry            `json:"listed,omitempty"`
	Installed  []*pb.Skill                     `json:"installed,omitempty"`
	Local      []*skillspec.LocalInstallResult `json:"local,omitempty"`
}

func (r skillsAddResult) MarshalJSON() ([]byte, error) {
	var installed []json.RawMessage
	if len(r.Installed) > 0 {
		var err error
		installed, err = marshalSkillsProtoJSON(r.Installed)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		Source     string                          `json:"source"`
		SourceType string                          `json:"sourceType"`
		Commit     string                          `json:"commit,omitempty"`
		Listed     []skillsPreviewEntry            `json:"listed,omitempty"`
		Installed  []json.RawMessage               `json:"installed,omitempty"`
		Local      []*skillspec.LocalInstallResult `json:"local,omitempty"`
	}{
		Source:     r.Source,
		SourceType: r.SourceType,
		Commit:     r.Commit,
		Listed:     r.Listed,
		Installed:  installed,
		Local:      r.Local,
	})
}

func (r *skillsAddResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Source     string                          `json:"source"`
		SourceType string                          `json:"sourceType"`
		Commit     string                          `json:"commit,omitempty"`
		Listed     []skillsPreviewEntry            `json:"listed,omitempty"`
		Installed  []json.RawMessage               `json:"installed,omitempty"`
		Local      []*skillspec.LocalInstallResult `json:"local,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	installed, err := unmarshalSkillsProtoJSON(raw.Installed)
	if err != nil {
		return err
	}
	r.Source = raw.Source
	r.SourceType = raw.SourceType
	r.Commit = raw.Commit
	r.Listed = raw.Listed
	r.Installed = installed
	r.Local = raw.Local
	return nil
}

type skillsPreviewEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
	Subpath     string `json:"subpath,omitempty"`
}

func (c *skillsAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	cwd, err := d.getwd()
	if err != nil {
		return nil, err
	}
	wanted := slices.Clone(c.Skill)
	if c.All && len(wanted) == 0 {
		wanted = []string{"*"}
	}
	res, err := skillspec.FetchSource(ctx, d.http, c.Source, skillspec.FetchOptions{
		Cwd:             cwd,
		GitHubHost:      d.getenv("GH_HOST"),
		Ref:             c.Ref,
		IncludeInternal: len(wanted) > 0 && !slices.Contains(wanted, "*"),
		FullDepth:       c.FullDepth,
	})
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "fetch skills from %q: %v", c.Source, err)
	}
	if len(res.Skills) == 0 {
		return nil, api.Errorf(api.CodeNotFound, "no valid skills (SKILL.md with name & description) found in %q", c.Source).
			WithSuggestion("pass --full-depth to scan nested subdirectories")
	}

	if res.Parsed.SkillFilter != "" {
		wanted = append(wanted, res.Parsed.SkillFilter)
	}

	if c.List {
		out := skillsAddResult{
			Source:     res.Source,
			SourceType: res.SourceType,
			Commit:     res.Commit,
		}
		for _, sk := range res.Skills {
			out.Listed = append(out.Listed, skillsPreviewEntry{
				Name:        sk.Name,
				Description: sk.Description,
				Files:       len(sk.Files),
				Subpath:     sk.Subpath,
			})
		}
		return out, nil
	}

	selected := skillspec.FilterSkills(res.Skills, wanted)
	if len(selected) == 0 {
		var avail []string
		for _, sk := range res.Skills {
			avail = append(avail, sk.Name)
		}
		return nil, api.Errorf(api.CodeNotFound, "no skills matched %v in %q", wanted, c.Source).
			WithSuggestion("available skills: " + strings.Join(avail, ", "))
	}

	if c.Rename != "" {
		if len(selected) != 1 {
			return nil, api.Errorf(api.CodeInvalidArgs, "--rename works with exactly one selected skill (matched %d)", len(selected)).
				WithSuggestion("select one skill with -s/--skill NAME or source@skill")
		}
	}

	out := skillsAddResult{
		Source:     res.Source,
		SourceType: res.SourceType,
		Commit:     res.Commit,
	}

	doLocal := c.Local || len(c.Agent) > 0 || c.Copy
	cl, cp, hasController, err := tryControllerClients(d)
	if err != nil {
		return nil, err
	}
	if !hasController && !doLocal {
		return nil, api.Errorf(api.CodeNotInitialized, "this machine has no controller").
			WithSuggestion("run `hotbed login https://...`, set HOTBED_CONTROLLER, or pass --local to install only into local agent directories")
	}

	var sessID string
	var from *pb.AskedFrom
	if hasController {
		sf := sessionFlag{Session: c.Session}
		sessID, _ = sf.optional(ctx, d)
		from, _ = d.askedFrom(ctx)
	}

	nowMs := time.Now().UnixMilli()
	for i := range selected {
		sk := selected[i]
		originalName := sk.Name
		if c.Rename != "" {
			if err := skillspec.RenameSkill(&sk, c.Rename); err != nil {
				return nil, api.Errorf(api.CodeInvalidArgs, "--rename %q: %v", c.Rename, err)
			}
		}

		if hasController {
			pbFiles := make([]*pb.SkillFile, 0, len(sk.Files))
			for _, f := range sk.Files {
				pbFiles = append(pbFiles, &pb.SkillFile{
					Path:     f.Path,
					Content:  f.Content,
					MimeType: f.MIMEType,
				})
			}
			srcMeta := &pb.SkillSource{
				SourceType:  res.SourceType,
				Source:      res.Source,
				SourceUrl:   res.SourceURL,
				Ref:         res.Ref,
				Commit:      res.Commit,
				Subpath:     sk.Subpath,
				ContentHash: sk.ContentHash,
				SyncedAt:    nowMs,
			}
			if res.Subpath != "" {
				srcMeta.Subpath = res.Subpath
			}
			if originalName != sk.Name {
				srcMeta.OriginalName = originalName
			}
			project := c.Project
			if !c.Global && project == "" && sessID == "" {
				project = strings.TrimSpace(d.getenv("HOTBED_PROJECT"))
			}
			req := &pb.PutSkillRequest{
				Files:           pbFiles,
				Global:          c.Global,
				Project:         project,
				Repo:            c.Repo,
				Tags:            c.Tag,
				Source:          srcMeta,
				Rationale:       c.Rationale,
				Model:           strings.TrimSpace(c.Model),
				SessionId:       sessID,
				From:            from,
				T3CodeThreadId:  d.t3codeThreadID(),
				Name:            sk.Name,
				FrontmatterJson: sk.FrontmatterJSON,
			}
			// If neither --global nor -p nor HOTBED_PROJECT nor session is set, default to global.
			if !req.Global && req.Project == "" && req.SessionId == "" {
				req.Global = true
			}
			saved, err := cl.skills.PutSkill(ctx, req)
			if err != nil {
				return nil, err
			}
			cp.fill(saved)
			out.Installed = append(out.Installed, saved)
		}

		if doLocal {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, herr
			}
			reqAgents := c.Agent
			if c.All && len(reqAgents) == 0 {
				reqAgents = []string{"*"}
			}
			agents, aerr := skillspec.ResolveAgents(home, reqAgents)
			if aerr != nil {
				return nil, api.Errorf(api.CodeInvalidArgs, "%v", aerr)
			}
			lres, lerr := skillspec.InstallToAgents(sk.Name, sk.Files, skillspec.LocalInstallOptions{
				Global: c.Global,
				Copy:   c.Copy,
				Cwd:    cwd,
				Home:   home,
				Agents: agents,
			})
			if lerr != nil {
				return nil, lerr
			}
			out.Local = append(out.Local, lres)
		}
	}

	return out, nil
}

// ---- install ----

type skillsInstallCmd struct {
	Source     string   `arg:"" optional:"" help:"Optional npx skills source to install (behaves like skills add; omit to install built-in guides or --from-hotbed skills)."`
	Dir        string   `type:"path" help:"Directory to write built-in guides into (default ~/.claude/skills)."`
	FromHotbed bool     `name:"from-hotbed" help:"Materialize active hotbed skills into local coding agent directories."`
	Global     bool     `short:"g" help:"Install to global user directories (~/.agents/skills + agent global dirs) or global hotbed scope."`
	Project    string   `short:"p" help:"Project scope."`
	Skill      []string `short:"s" name:"skill" help:"Specific skill name(s) to install (use '*' for all). Repeatable."`
	Agent      []string `short:"a" name:"agent" help:"Target local coding agent(s) (e.g. claude-code, codex, cursor, or '*')."`
	Copy       bool     `help:"Copy files instead of symlinking into agent directories."`
	Rename     string   `help:"Rename a single installed skill."`
	All        bool     `help:"Install all skills to all agents."`
	FullDepth  bool     `name:"full-depth" help:"Search all subdirectories when installing from a source."`
	Ref        string   `help:"Git branch, tag, or commit SHA when installing from a source."`
	Yes        bool     `short:"y" help:"Skip confirmation prompts."`
	Session    string   `name:"session" env:"HOTBED_SESSION" help:"Session ID."`
}

type skillsLocalSyncResult struct {
	Installed []*skillspec.LocalInstallResult `json:"installed"`
}

func (c *skillsInstallCmd) Exec(ctx context.Context, d deps) (any, error) {
	// 1. Source provided: delegate to skillsAddCmd (matching `npx skills install <source>`).
	if strings.TrimSpace(c.Source) != "" {
		add := &skillsAddCmd{
			Source:    c.Source,
			Global:    c.Global,
			Project:   c.Project,
			Skill:     c.Skill,
			Rename:    c.Rename,
			All:       c.All,
			FullDepth: c.FullDepth,
			Ref:       c.Ref,
			Yes:       c.Yes,
			Session:   c.Session,
			Local:     len(c.Agent) > 0 || c.Copy || c.All,
			Agent:     c.Agent,
			Copy:      c.Copy,
		}
		return add.Exec(ctx, d)
	}

	// 2. Materialize active Hotbed skills into local agent directories.
	if c.FromHotbed || len(c.Agent) > 0 || len(c.Skill) > 0 {
		cl, _, ok, err := tryControllerClients(d)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, api.Errorf(api.CodeNotInitialized, "this machine has no controller").
				WithSuggestion("run `hotbed login https://...` or set HOTBED_CONTROLLER")
		}
		cwd, err := d.getwd()
		if err != nil {
			return nil, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		reqAgents := c.Agent
		if c.All && len(reqAgents) == 0 {
			reqAgents = []string{"*"}
		}
		agents, err := skillspec.ResolveAgents(home, reqAgents)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "%v", err)
		}

		listReq := &pb.ListSkillsRequest{}
		if c.Project != "" {
			listReq.ProjectScope = &pb.ListSkillsRequest_Project{Project: c.Project}
		} else if sessID, err := (sessionFlag{Session: c.Session}).optional(ctx, d); err == nil && sessID != "" {
			listReq.SessionId = sessID
		} else if remote, err := d.checkoutRemote(ctx); err == nil && remote != "" {
			listReq.Remote = remote
		} else if p := strings.TrimSpace(d.getenv("HOTBED_PROJECT")); p != "" && !c.Global {
			listReq.ProjectScope = &pb.ListSkillsRequest_Project{Project: p}
		} else {
			listReq.Scope = pb.SkillScope_SKILL_SCOPE_GLOBAL
		}
		listed, err := cl.skills.ListSkills(ctx, listReq)
		if err != nil {
			return nil, err
		}
		var out skillsLocalSyncResult
		for _, sk := range listed.Skills {
			if len(c.Skill) > 0 && !slices.Contains(c.Skill, "*") && !matchesSkillFilter(sk.Name, c.Skill) {
				continue
			}
			got, err := cl.skills.GetSkill(ctx, &pb.GetSkillRequest{Ref: strconv.Itoa(int(sk.Id)), AllFiles: true})
			if err != nil {
				return nil, err
			}
			entries := make([]skillspec.FileEntry, 0, len(got.Files))
			for _, f := range got.Files {
				entries = append(entries, skillspec.FileEntry{
					Path:     f.Path,
					Content:  f.Content,
					MIMEType: f.MimeType,
				})
			}
			lres, err := skillspec.InstallToAgents(sk.Name, entries, skillspec.LocalInstallOptions{
				Global: c.Global,
				Copy:   c.Copy,
				Cwd:    cwd,
				Home:   home,
				Agents: agents,
			})
			if err != nil {
				return nil, err
			}
			out.Installed = append(out.Installed, lres)
		}
		return out, nil
	}

	// 3. Default (no args): install compiled-in hotbed guide to ~/.claude/skills
	// (preserves exact backward compatibility with `hotbed upgrade` reexec).
	dir := c.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".claude", "skills")
	}
	wrote, err := skills.Install(dir)
	return map[string]any{"dir": tildePath(dir), "files": wrote}, err
}

func matchesSkillFilter(name string, filters []string) bool {
	nl := strings.ToLower(name)
	nn := skillspec.NormalizeSkillName(name)
	for _, f := range filters {
		if strings.ToLower(strings.TrimSpace(f)) == nl || skillspec.NormalizeSkillName(f) == nn {
			return true
		}
	}
	return false
}

// ---- find ----

type skillsFindCmd struct {
	Query []string `arg:"" optional:"" help:"Search query keywords."`
	Owner string   `help:"Restrict search to repositories owned by this GitHub user/org."`
}

type skillsFindResult struct {
	Query  string                  `json:"query"`
	Owner  string                  `json:"owner,omitempty"`
	Skills []skillspec.SearchSkill `json:"skills"`
}

func (c *skillsFindCmd) Exec(ctx context.Context, d deps) (any, error) {
	q := strings.TrimSpace(strings.Join(c.Query, " "))
	found, err := skillspec.SearchSkillsAPI(ctx, d.http, q, c.Owner, d.getenv("SKILLS_API_URL"))
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "search skills.sh: %v", err)
	}
	if found == nil {
		found = []skillspec.SearchSkill{}
	}
	return skillsFindResult{Query: q, Owner: c.Owner, Skills: found}, nil
}

// ---- check & update ----

type skillsCheckCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Names       []string `arg:"" optional:"" help:"Specific skill name(s) to check (default: all active skills in scope)."`
	Global      bool     `short:"g" help:"Check only global skills."`
	AllProjects bool     `name:"all-projects" help:"Check skills across global and every project."`
}

type skillCheckItem struct {
	ID          int32  `json:"id"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Source      string `json:"source,omitempty"`
	Status      string `json:"status"` // "up-to-date", "update-available", "updated", "untracked", "missing-upstream", "error"
	CurrentHash string `json:"currentHash,omitempty"`
	LatestHash  string `json:"latestHash,omitempty"`
	Commit      string `json:"commit,omitempty"`
	LatestRef   string `json:"latestCommit,omitempty"`
	Error       string `json:"error,omitempty"`
}

type skillsCheckResult struct {
	Skills           []skillCheckItem `json:"skills"`
	UpdatesAvailable int              `json:"updatesAvailable"`
	Updated          int              `json:"updated,omitempty"`
}

func (c *skillsCheckCmd) Exec(ctx context.Context, d deps) (any, error) {
	return runSkillSyncCheck(ctx, d, c.controllerClient, c.sessionFlag, c.projectFlag, c.Names, c.Global, c.AllProjects, false)
}

type skillsUpdateCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Names       []string `arg:"" optional:"" help:"Specific skill name(s) to update (default: all active skills in scope)."`
	Global      bool     `short:"g" help:"Update only global skills."`
	AllProjects bool     `name:"all-projects" help:"Update skills across global and every project."`
}

func (c *skillsUpdateCmd) Exec(ctx context.Context, d deps) (any, error) {
	return runSkillSyncCheck(ctx, d, c.controllerClient, c.sessionFlag, c.projectFlag, c.Names, c.Global, c.AllProjects, true)
}

func runSkillSyncCheck(
	ctx context.Context,
	d deps,
	cc controllerClient,
	sf sessionFlag,
	pf projectFlag,
	names []string,
	globalOnly, allProjects, applyUpdates bool,
) (skillsCheckResult, error) {
	cl, err := cc.clients(d.http)
	if err != nil {
		return skillsCheckResult{}, err
	}
	req := &pb.ListSkillsRequest{}
	if globalOnly {
		req.Scope = pb.SkillScope_SKILL_SCOPE_GLOBAL
	} else {
		sc, err := pf.listing(ctx, d, sf, allProjects)
		if err != nil {
			return skillsCheckResult{}, err
		}
		if sc.all {
			req.ProjectScope = &pb.ListSkillsRequest_AllProjects{AllProjects: true}
		} else if sc.project != "" {
			req.ProjectScope = &pb.ListSkillsRequest_Project{Project: sc.project}
		}
		req.SessionId = sc.session
		req.Remote = sc.remote
	}

	listed, err := cl.skills.ListSkills(ctx, req)
	if err != nil {
		return skillsCheckResult{}, err
	}

	var targets []*pb.Skill
	for _, s := range listed.Skills {
		if len(names) > 0 && !matchesSkillFilter(s.Name, names) {
			continue
		}
		targets = append(targets, s)
	}

	cwd, _ := d.getwd()
	sessID, _ := sf.optional(ctx, d)
	type fetchKey struct{ url, ref string }
	cache := map[fetchKey]*skillspec.FetchResult{}
	cacheErr := map[fetchKey]error{}

	out := skillsCheckResult{Skills: make([]skillCheckItem, 0, len(targets))}
	nowMs := time.Now().UnixMilli()

	for _, s := range targets {
		scope := s.Project
		if scope == "" {
			scope = "global"
		}
		item := skillCheckItem{
			ID:    s.Id,
			Name:  s.Name,
			Scope: scope,
		}
		if s.Source == nil || s.Source.SourceUrl == "" || s.Source.SourceType == "manual" {
			item.Status = "untracked"
			out.Skills = append(out.Skills, item)
			continue
		}
		item.Source = s.Source.Source
		item.CurrentHash = s.Source.ContentHash
		item.Commit = s.Source.Commit

		key := fetchKey{url: s.Source.SourceUrl, ref: s.Source.Ref}
		fetched, ok := cache[key]
		ferr := cacheErr[key]
		if !ok && ferr == nil {
			fetched, ferr = skillspec.FetchSource(ctx, d.http, s.Source.SourceUrl, skillspec.FetchOptions{
				Cwd:             cwd,
				GitHubHost:      d.getenv("GH_HOST"),
				Ref:             s.Source.Ref,
				IncludeInternal: true,
			})
			cache[key] = fetched
			cacheErr[key] = ferr
		}
		if ferr != nil {
			item.Status = "error"
			item.Error = ferr.Error()
			out.Skills = append(out.Skills, item)
			continue
		}

		lookupName := s.Source.OriginalName
		if lookupName == "" {
			lookupName = s.Name
		}
		matches := skillspec.FilterSkills(fetched.Skills, []string{lookupName})
		if len(matches) == 0 {
			item.Status = "missing-upstream"
			out.Skills = append(out.Skills, item)
			continue
		}
		latest := matches[0]
		if s.Source.OriginalName != "" && s.Source.OriginalName != s.Name {
			if err := skillspec.RenameSkill(&latest, s.Name); err != nil {
				item.Status = "error"
				item.Error = err.Error()
				out.Skills = append(out.Skills, item)
				continue
			}
		}
		item.LatestHash = latest.ContentHash
		item.LatestRef = fetched.Commit

		changed := (s.Source.ContentHash != "" && latest.ContentHash != s.Source.ContentHash) ||
			(s.Source.Commit != "" && fetched.Commit != "" && fetched.Commit != s.Source.Commit)
		if !changed {
			item.Status = "up-to-date"
			out.Skills = append(out.Skills, item)
			continue
		}

		if !applyUpdates {
			item.Status = "update-available"
			out.UpdatesAvailable++
			out.Skills = append(out.Skills, item)
			continue
		}

		pbFiles := make([]*pb.SkillFile, 0, len(latest.Files))
		for _, f := range latest.Files {
			pbFiles = append(pbFiles, &pb.SkillFile{
				Path:     f.Path,
				Content:  f.Content,
				MimeType: f.MIMEType,
			})
		}
		_, err := cl.skills.PutSkill(ctx, &pb.PutSkillRequest{
			Files:   pbFiles,
			Global:  s.Scope == pb.SkillScope_SKILL_SCOPE_GLOBAL,
			Project: s.Project,
			Repo:    s.Repo,
			Tags:    s.Tags,
			Source: &pb.SkillSource{
				SourceType:   s.Source.SourceType,
				Source:       s.Source.Source,
				SourceUrl:    s.Source.SourceUrl,
				Ref:          s.Source.Ref,
				Commit:       fetched.Commit,
				Subpath:      s.Source.Subpath,
				OriginalName: s.Source.OriginalName,
				ContentHash:  latest.ContentHash,
				SyncedAt:     nowMs,
			},
			Rationale:       "updated from upstream source",
			SessionId:       sessID,
			IfRevision:      s.Revision,
			Name:            latest.Name,
			FrontmatterJson: latest.FrontmatterJSON,
		})
		if err != nil {
			item.Status = "error"
			item.Error = err.Error()
		} else {
			item.Status = "updated"
			out.Updated++
		}
		out.Skills = append(out.Skills, item)
	}
	return out, nil
}

// ---- init ----

type skillsInitCmd struct {
	Name string `arg:"" optional:"" help:"Skill name (creates ./<name>/SKILL.md; omit to create ./SKILL.md using the current directory name)."`
}

type skillsInitResult struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (c *skillsInitCmd) Exec(_ context.Context, d deps) (any, error) {
	cwd, err := d.getwd()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(c.Name)
	targetDir := cwd
	if name != "" {
		if err := skillspec.ValidateName(name); err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "%v", err)
		}
		targetDir = filepath.Join(cwd, name)
	} else {
		name = skillspec.NormalizeSkillName(filepath.Base(cwd))
		if name == "" {
			name = "my-skill"
		}
	}
	skillPath := filepath.Join(targetDir, "SKILL.md")
	if _, err := os.Stat(skillPath); err == nil {
		return nil, api.Errorf(api.CodeConflict, "%s already exists", skillPath)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, err
	}
	template := fmt.Sprintf(`---
name: %s
description: Describe what this skill does and when an agent should use it.
---

# %s

Instructions for agents using this skill.
`, name, name)
	if err := os.WriteFile(skillPath, []byte(template), 0o644); err != nil {
		return nil, err
	}
	return skillsInitResult{Name: name, Path: skillPath}, nil
}

// ---- edit, promote, archive, restore, rm, move ----

func resolveSkillID(ctx context.Context, cl clients, d deps, sf sessionFlag, pf projectFlag, ref string) (int32, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if id, err := strconv.Atoi(trimmed); err == nil && id > 0 {
		return int32(id), nil
	}
	req := &pb.ShowSkillRequest{Ref: ref, Project: pf.Project}
	if req.Project == "" {
		if sessID, err := sf.optional(ctx, d); err == nil && sessID != "" {
			req.SessionId = sessID
		} else if remote, err := d.checkoutRemote(ctx); err == nil && remote != "" {
			req.Remote = remote
		}
	}
	sk, err := cl.skills.ShowSkill(ctx, req)
	if err != nil {
		return 0, err
	}
	return sk.Id, nil
}

type skillsEditCmd struct {
	controllerClient
	sessionFlag
	Ref         string   `arg:"" help:"Skill ID or name to edit."`
	Global      bool     `short:"g" help:"Change scope to global (shared across all projects)."`
	Project     string   `short:"p" help:"Move to this project (mutually exclusive with --global)."`
	Repo        *string  `help:"Set or clear (\"\") the optional repo within a project-scoped skill."`
	Tag         []string `help:"Tag(s) to add (or replace with --replace-tags). Repeatable."`
	ReplaceTags bool     `name:"replace-tags" help:"Replace all tags with --tag (omit --tag to clear)."`
	Rationale   *string  `help:"Set or clear (\"\") the provenance rationale."`
	IfRevision  int64    `name:"if-revision" help:"Refuse with CONFLICT unless at this revision."`
}

func (c *skillsEditCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	lookupProj := strings.TrimSpace(d.getenv("HOTBED_PROJECT"))
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, projectFlag{Project: lookupProj}, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	return cl.skills.ReviseSkill(ctx, &pb.ReviseSkillRequest{
		Id:          id,
		Global:      c.Global,
		Project:     c.Project,
		Repo:        c.Repo,
		Tags:        c.Tag,
		ReplaceTags: c.ReplaceTags,
		Rationale:   c.Rationale,
		SessionId:   sessID,
		IfRevision:  c.IfRevision,
	})
}

type skillsPromoteCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref        string `arg:"" help:"Project-scoped skill ID or name to promote to global."`
	IfRevision int64  `name:"if-revision" help:"Refuse with CONFLICT unless at this revision."`
}

func (c *skillsPromoteCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, c.projectFlag, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	return cl.skills.ReviseSkill(ctx, &pb.ReviseSkillRequest{
		Id:         id,
		Global:     true,
		SessionId:  sessID,
		IfRevision: c.IfRevision,
	})
}

type skillsArchiveCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref        string `arg:"" help:"Active skill ID or name to archive."`
	Reason     string `help:"Why this skill is being archived."`
	IfRevision int64  `name:"if-revision" help:"Refuse with CONFLICT unless at this revision."`
}

func (c *skillsArchiveCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, c.projectFlag, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	return cl.skills.ArchiveSkill(ctx, &pb.ArchiveSkillRequest{
		Id:         id,
		Reason:     c.Reason,
		SessionId:  sessID,
		IfRevision: c.IfRevision,
	})
}

type skillsRestoreCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref        string `arg:"" help:"Archived skill ID or name to restore."`
	IfRevision int64  `name:"if-revision" help:"Refuse with CONFLICT unless at this revision."`
}

func (c *skillsRestoreCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, c.projectFlag, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	return cl.skills.RestoreSkill(ctx, &pb.RestoreSkillRequest{
		Id:         id,
		SessionId:  sessID,
		IfRevision: c.IfRevision,
	})
}

type skillsRmCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref        string `arg:"" help:"Skill ID or name to permanently remove."`
	IfRevision int64  `name:"if-revision" help:"Refuse with CONFLICT unless at this revision."`
}

func (c *skillsRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, c.projectFlag, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	return cl.skills.DeleteSkill(ctx, &pb.DeleteSkillRequest{
		Id:         id,
		SessionId:  sessID,
		IfRevision: c.IfRevision,
	})
}

type skillsMoveCmd struct {
	controllerClient
	sessionFlag
	projectFlag
	Ref    string `arg:"" help:"Active skill ID or name to reorder."`
	Before int32  `help:"Move just before this skill ID."`
	First  bool   `help:"Move to the top of startup order."`
	Last   bool   `help:"Move to the end of startup order."`
}

func (c *skillsMoveCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	id, err := resolveSkillID(ctx, cl, d, c.sessionFlag, c.projectFlag, c.Ref)
	if err != nil {
		return nil, err
	}
	sessID, _ := c.sessionFlag.optional(ctx, d)
	req := &pb.MoveSkillRequest{Id: id, SessionId: sessID}
	switch {
	case c.Before > 0:
		req.Place = &pb.MoveSkillRequest_Before{Before: c.Before}
	case c.First:
		req.Place = &pb.MoveSkillRequest_First{First: true}
	case c.Last:
		req.Place = &pb.MoveSkillRequest_Last{Last: true}
	default:
		return nil, api.Errorf(api.CodeInvalidArgs, "pass --before ID, --first, or --last")
	}
	return cl.skills.MoveSkill(ctx, req)
}

// ---- plain-text formatting for skill reports ----

func formatSkillReport(data any) (string, bool) {
	switch v := data.(type) {
	case skillsListResult:
		return formatSkillsListResult(v), true
	case skillsAddResult:
		return formatSkillsAddResult(v), true
	case skillsLocalSyncResult:
		return formatSkillsLocalSyncResult(v), true
	case skillsFindResult:
		return formatSkillsFindResult(v), true
	case skillsCheckResult:
		return formatSkillsCheckResult(v), true
	case skillsInitResult:
		return fmt.Sprintf("created %s (%s)\n", v.Path, v.Name), true
	case *pb.Skill:
		// ShowSkill populates history or files; check if it looks like a ShowSkill report.
		if len(v.Files) > 0 && len(v.History) > 0 {
			return formatSkillShow(v), true
		}
	}
	return "", false
}

func formatSkillsListResult(r skillsListResult) string {
	var sb strings.Builder
	for _, s := range r.Skills {
		scope := s.Project
		if scope == "" {
			scope = "global"
		}
		fmt.Fprintf(&sb, "#%-4d  %-8s  %-12s  %-28s  %df  %s\n", s.Id, enumWord(s.State), scope, s.Name, s.FileCount, apicmd.Short(60, s.Description))
	}
	for _, b := range r.Builtin {
		fmt.Fprintf(&sb, "builtin  %-28s  %s\n", b.Name, apicmd.Short(60, b.Description))
	}
	return sb.String()
}

func formatSkillShow(s *pb.Skill) string {
	var sb strings.Builder
	scope := s.Project
	if scope == "" {
		scope = "global"
	}
	if s.Repo != "" {
		scope += "/" + s.Repo
	}
	fmt.Fprintf(&sb, "#%d  %s  rev=%d  [%s]  %s\n", s.Id, enumWord(s.State), s.Revision, scope, s.Name)
	fmt.Fprintf(&sb, "uri         %s\n", s.Uri)
	fmt.Fprintf(&sb, "description %s\n", s.Description)
	if len(s.Tags) > 0 {
		fmt.Fprintf(&sb, "tags        %s\n", strings.Join(s.Tags, ", "))
	}
	if s.Source != nil && s.Source.Source != "" {
		fmt.Fprintf(&sb, "source      %s (%s)", s.Source.Source, s.Source.SourceType)
		if s.Source.Ref != "" {
			fmt.Fprintf(&sb, " ref=%s", s.Source.Ref)
		}
		if s.Source.Commit != "" {
			c := s.Source.Commit
			if len(c) > 12 {
				c = c[:12]
			}
			fmt.Fprintf(&sb, " commit=%s", c)
		}
		if s.Source.OriginalName != "" {
			fmt.Fprintf(&sb, " original=%s", s.Source.OriginalName)
		}
		sb.WriteByte('\n')
	}
	if s.Provenance != nil && s.Provenance.Rationale != "" {
		fmt.Fprintf(&sb, "rationale   %s\n", s.Provenance.Rationale)
	}
	if s.FrontmatterJson != "" {
		var fm map[string]any
		if json.Unmarshal([]byte(s.FrontmatterJson), &fm) == nil {
			if compat, ok := fm["compatibility"].(string); ok && compat != "" {
				fmt.Fprintf(&sb, "compat      %s\n", compat)
			}
			if lic, ok := fm["license"].(string); ok && lic != "" {
				fmt.Fprintf(&sb, "license     %s\n", lic)
			}
		}
	}
	fmt.Fprintf(&sb, "files       %d (%s)\n", s.FileCount, formatBytes(s.TotalBytes))
	for _, f := range s.Files {
		shortDigest := f.Digest
		if len(shortDigest) > 19 {
			shortDigest = shortDigest[:19]
		}
		fmt.Fprintf(&sb, "  %-36s  %8s  %s  %s\n", f.Path, formatBytes(f.Size), shortDigest, f.MimeType)
	}
	if s.CockpitUrl != "" {
		fmt.Fprintf(&sb, "cockpit     %s\n", s.CockpitUrl)
	}
	return sb.String()
}

func formatSkillsAddResult(r skillsAddResult) string {
	var sb strings.Builder
	if len(r.Listed) > 0 {
		fmt.Fprintf(&sb, "Skills in %s (%d):\n", r.Source, len(r.Listed))
		for _, e := range r.Listed {
			fmt.Fprintf(&sb, "  %-28s  %df  %s\n", e.Name, e.Files, e.Description)
		}
		return sb.String()
	}
	for _, s := range r.Installed {
		scope := s.Project
		if scope == "" {
			scope = "global"
		}
		fmt.Fprintf(&sb, "#%-4d  [%s]  %s  (%d files, %s)  %s\n", s.Id, scope, s.Name, s.FileCount, formatBytes(s.TotalBytes), s.Uri)
		if s.CockpitUrl != "" {
			fmt.Fprintf(&sb, "  %s\n", s.CockpitUrl)
		}
	}
	for _, l := range r.Local {
		fmt.Fprintf(&sb, "local  %s -> %s (%s, %d files)", l.SkillName, tildePath(l.CanonicalDir), l.Mode, l.Files)
		if len(l.AgentDirs) > 0 {
			fmt.Fprintf(&sb, " + %d agent dir(s)", len(l.AgentDirs))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func formatSkillsLocalSyncResult(r skillsLocalSyncResult) string {
	if len(r.Installed) == 0 {
		return "(no skills installed)\n"
	}
	var sb strings.Builder
	for _, l := range r.Installed {
		fmt.Fprintf(&sb, "%-28s  %s (%s, %d files)", l.SkillName, tildePath(l.CanonicalDir), l.Mode, l.Files)
		if len(l.AgentDirs) > 0 {
			fmt.Fprintf(&sb, " + %d agent dir(s)", len(l.AgentDirs))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func formatSkillsFindResult(r skillsFindResult) string {
	if len(r.Skills) == 0 {
		return "(no matching skills found)\n"
	}
	var sb strings.Builder
	for _, s := range r.Skills {
		ref := s.Source
		if ref != "" && s.Name != "" {
			ref += "@" + s.Name
		} else if ref == "" {
			ref = s.Slug
		}
		fmt.Fprintf(&sb, "%-48s  %6d installs\n", ref, s.Installs)
	}
	return sb.String()
}

func formatSkillsCheckResult(r skillsCheckResult) string {
	if len(r.Skills) == 0 {
		return "(no skills in scope)\n"
	}
	var sb strings.Builder
	for _, s := range r.Skills {
		fmt.Fprintf(&sb, "#%-4d  %-12s  %-26s  %-16s", s.ID, s.Scope, s.Name, s.Status)
		if s.Source != "" {
			fmt.Fprintf(&sb, "  %s", s.Source)
		}
		if s.Error != "" {
			fmt.Fprintf(&sb, "  (%s)", s.Error)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
