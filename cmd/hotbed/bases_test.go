package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestProjectBasesCLIAndMCP(t *testing.T) {
	c := newFleet(t, 2)
	origin := upstream(t)
	c.addRepo(origin)

	// Build a warm base on the first node (asahimbp) and another on mini.
	c.ok(nil, "session", "start", "base-fox")
	ws1 := c.workspace("base-fox", &pb.Placement{Node: "asahimbp"})
	ws2 := c.workspace("base-fox", &pb.Placement{Node: "mini"})
	if ws1.BaseId != "app-g1" || ws2.BaseId != "app-g1" {
		t.Fatalf("workspaces = %s / %s, want app-g1 on both", ws1.BaseId, ws2.BaseId)
	}

	// Add a failing setup step and trigger a new freshen on asahimbp so g2 fails
	// while g1 stays warm.
	c.ok(nil, "step", "set", "-p", "app", "broken-step", "--", "exit 7")
	c.ok(nil, "session", "start", "base-owl")
	wsFail := c.workspace("base-owl", &pb.Placement{Node: "asahimbp"})
	if wsFail.BaseId != "app-g1" {
		t.Fatalf("fallback workspace base = %s, want app-g1", wsFail.BaseId)
	}

	// Wait until the controller's views reflect g2 failed on asahimbp.
	c.eventually("asahimbp g2 failed visible in project bases", func() bool {
		var res pb.ListBasesResponse
		c.ok(&res, "project", "bases", "app", "--node", "asahimbp")
		for _, nb := range res.Bases {
			if nb.Base.GetId() == "app-g2" && nb.Base.GetState() == pb.BaseState_BASE_STATE_FAILED {
				return true
			}
		}
		return false
	})

	// 1. All projects (no positional arg): newest generation first per node.
	var all pb.ListBasesResponse
	c.ok(&all, "project", "bases")
	var got []string
	for _, nb := range all.Bases {
		got = append(got, nb.Node+"/"+nb.Base.Id+"/"+nb.Base.State.String())
	}
	want := []string{
		"asahimbp/app-g2/BASE_STATE_FAILED",
		"asahimbp/app-g1/BASE_STATE_WARM",
		"mini/app-g1/BASE_STATE_WARM",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("project bases = %v, want %v", got, want)
	}
	if all.Bases[0].FailedStep != "broken-step" {
		t.Errorf("failed base FailedStep = %q, want broken-step", all.Bases[0].FailedStep)
	}
	if all.Bases[0].Base.Command == "" {
		t.Errorf("failed base missing freshen command ID")
	}

	// 2. Plain-text output shows node, project, base ID, state, commit, cmd=, and failed=.
	code, plain, stderr := c.hotbed("project", "bases", "app")
	if code != exitOK {
		t.Fatalf("project bases plain exited %d: %s", code, stderr)
	}
	var g2Line string
	for _, line := range strings.Split(strings.TrimSpace(plain), "\n") {
		if strings.Contains(line, "app-g2") {
			g2Line = line
			break
		}
	}
	g2Cols := strings.Fields(g2Line)
	if len(g2Cols) < 4 || g2Cols[0] != "asahimbp" || g2Cols[1] != "app" || g2Cols[2] != "app-g2" || g2Cols[3] != "failed" ||
		!strings.Contains(g2Line, "failed=broken-step") ||
		!strings.Contains(g2Line, "cmd="+all.Bases[0].Base.Command) ||
		!strings.Contains(g2Line, "app@") {
		t.Errorf("unexpected plain output:\n%s", plain)
	}

	// 3. Filter by --node and --state.
	var warmOnMini pb.ListBasesResponse
	c.ok(&warmOnMini, "project", "bases", "app", "--node", "mini", "--state", "warm")
	if len(warmOnMini.Bases) != 1 || warmOnMini.Bases[0].Node != "mini" || warmOnMini.Bases[0].Base.Id != "app-g1" {
		t.Fatalf("project bases --node mini --state warm = %+v", warmOnMini.Bases)
	}

	var onlyFailed pb.ListBasesResponse
	c.ok(&onlyFailed, "project", "bases", "--state", "failed")
	if len(onlyFailed.Bases) != 1 || onlyFailed.Bases[0].Base.Id != "app-g2" {
		t.Fatalf("project bases --state failed = %+v", onlyFailed.Bases)
	}

	// 4. Repo ID lookup when project name differs from repo ID.
	other := upstream(t)
	write(t, filepath.Join(other, "src/lib.txt"), "other\n")
	gitIn(t, other, "commit", "--quiet", "-am", "Other")
	c.ok(nil, "project", "add", "suite", "--repo", other)
	c.ok(nil, "repo", "add", "svc-repo", other, "-p", "suite")
	var suiteBases pb.ListBasesResponse
	c.ok(&suiteBases, "project", "bases", "svc-repo")
	if len(suiteBases.Bases) != 0 {
		t.Fatalf("project bases svc-repo = %+v, want empty", suiteBases.Bases)
	}

	// 5. Unknown project returns NOT_FOUND.
	if _, e := c.fails("project", "bases", "no-such-proj"); e.Code != api.CodeNotFound {
		t.Fatalf("project bases unknown = %+v, want NOT_FOUND", e)
	}

	// 6. MCP tool `project_bases`: optional project param, filters work end to end.
	t.Setenv("T3CODE_THREAD_ID", "thread-bases-mcp")
	m := c.mcp()
	defer m.stop()

	tools, err := m.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var toolSchema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	foundTool := false
	for _, tool := range tools.Tools {
		if tool.Name == "project_bases" {
			foundTool = true
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &toolSchema); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !foundTool {
		t.Fatalf("MCP tool project_bases not registered")
	}
	if len(toolSchema.Required) != 0 {
		t.Errorf("project_bases MCP required = %v, want empty (project is optional)", toolSchema.Required)
	}

	mcpAll := &pb.ListBasesResponse{}
	m.call(mcpAll, "project_bases", nil)
	if len(mcpAll.Bases) != 3 {
		t.Fatalf("MCP project_bases() = %d bases, want 3", len(mcpAll.Bases))
	}
	mcpFiltered := &pb.ListBasesResponse{}
	m.call(mcpFiltered, "project_bases", map[string]any{"project": "app", "node": "asahimbp", "state": "failed"})
	if len(mcpFiltered.Bases) != 1 || mcpFiltered.Bases[0].FailedStep != "broken-step" {
		t.Fatalf("MCP project_bases(filtered) = %+v", mcpFiltered.Bases)
	}
}
