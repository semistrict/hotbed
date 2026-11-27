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
	c := newMixedArchFleet(t)
	origin := upstream(t)
	c.addRepo(origin)

	// Build a warm base on the first node (asahimbp) and another on mini.
	c.ok(nil, "session", "start", "base-fox")
	ws1 := c.workspace("base-fox", &pb.Placement{Arch: "arm64"})
	ws2 := c.workspace("base-fox", &pb.Placement{Arch: "amd64"})
	if ws1.BaseId != "app-g1" || ws2.BaseId != "app-g1" {
		t.Fatalf("workspaces = %s / %s, want app-g1 on both", ws1.BaseId, ws2.BaseId)
	}

	// Add a failing setup step and trigger a new freshen on asahimbp so g2 fails
	// while g1 stays warm.
	c.ok(nil, "step", "set", "-p", "app", "broken-step", "--", "exit 7")
	c.ok(nil, "session", "start", "base-owl")
	wsFail := c.workspace("base-owl", &pb.Placement{Arch: "arm64"})
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

func TestFreshenCLIAndMCP(t *testing.T) {
	c := newMixedArchFleet(t)
	origin := upstream(t)
	c.addRepo(origin)
	c.ok(nil, "step", "set", "-p", "app", "prep-step", "--", "echo ok")

	// 1. Cold build on asahimbp (g1) and mini (g1).
	c.ok(nil, "session", "start", "fresh-fox")
	ws1 := c.workspace("fresh-fox", &pb.Placement{Arch: "arm64"})
	ws2 := c.workspace("fresh-fox", &pb.Placement{Arch: "amd64"})
	if ws1.BaseId != "app-g1" || ws2.BaseId != "app-g1" {
		t.Fatalf("workspaces = %s / %s, want app-g1", ws1.BaseId, ws2.BaseId)
	}

	// 2. Failing incremental freshen on asahimbp (g2), then scratch rebuild (g3).
	c.ok(nil, "step", "set", "-p", "app", "broken-step", "--", "exit 7")
	c.ok(nil, "session", "start", "fresh-owl")
	_ = c.workspace("fresh-owl", &pb.Placement{Arch: "arm64"})

	c.eventually("freshen ls reports cold g1 and failed g2 on asahimbp", func() bool {
		var res pb.ListFreshensResponse
		c.ok(&res, "freshen", "ls", "app", "--node", "asahimbp")
		var gotG1, gotG2 bool
		for _, f := range res.Freshens {
			if f.BaseId == "app-g1" && f.State == pb.FreshenState_FRESHEN_STATE_SUCCEEDED {
				gotG1 = true
			}
			if f.BaseId == "app-g2" && f.State == pb.FreshenState_FRESHEN_STATE_FAILED {
				gotG2 = true
			}
		}
		return gotG1 && gotG2
	})

	// Remove the broken step, trigger a scratch rebuild, and warm g3 on asahimbp.
	c.ok(nil, "step", "rm", "-p", "app", "broken-step")
	c.ok(nil, "project", "rebuild", "app")
	c.ok(nil, "session", "start", "fresh-elk")
	ws3 := c.workspace("fresh-elk", &pb.Placement{Arch: "arm64"})
	if ws3.BaseId != "app-g3" {
		t.Fatalf("rebuilt workspace base = %s, want app-g3", ws3.BaseId)
	}

	c.eventually("freshen ls reports cold g3 with scratch_epoch and preserves g2", func() bool {
		var res pb.ListFreshensResponse
		c.ok(&res, "freshen", "ls", "app", "--node", "asahimbp")
		var gotG3, gotG2 bool
		for _, f := range res.Freshens {
			if f.BaseId == "app-g3" && f.State == pb.FreshenState_FRESHEN_STATE_SUCCEEDED && f.Cold && f.ColdReason == "scratch_epoch" {
				gotG3 = true
			}
			if f.BaseId == "app-g2" && f.State == pb.FreshenState_FRESHEN_STATE_FAILED && f.FailedStep == "broken-step" {
				gotG2 = true
			}
		}
		return gotG3 && gotG2
	})

	// 3. Check ListFreshens summary, filters, and plain-text report.
	var ls pb.ListFreshensResponse
	c.ok(&ls, "freshen", "ls", "app")
	if ls.Summary == nil || ls.Summary.Total < 4 {
		t.Fatalf("freshen ls summary = %+v, want >= 4 total", ls.Summary)
	}
	if ls.Summary.Succeeded < 3 || ls.Summary.Failed < 1 || ls.Summary.Cold < 3 {
		t.Fatalf("unexpected summary counts: %+v", ls.Summary)
	}
	if len(ls.Summary.ByNodeProject) < 2 {
		t.Fatalf("byNodeProject = %+v, want >= 2", ls.Summary.ByNodeProject)
	}

	code, plain, stderr := c.hotbed("freshen", "ls", "app")
	if code != exitOK {
		t.Fatalf("freshen ls plain exited %d: %s", code, stderr)
	}
	for _, want := range []string{"Base freshens", "By node & project:", "Slowest steps:", "scratch_epoch", "failed=broken-step"} {
		if !strings.Contains(plain, want) {
			t.Errorf("freshen ls plain missing %q:\n%s", want, plain)
		}
	}

	// 4. Filter --cold and --state failed.
	var onlyFailed pb.ListFreshensResponse
	c.ok(&onlyFailed, "freshen", "ls", "--state", "failed")
	if len(onlyFailed.Freshens) != 1 || onlyFailed.Freshens[0].BaseId != "app-g2" {
		t.Fatalf("freshen ls --state failed = %+v", onlyFailed.Freshens)
	}
	g2CmdID := onlyFailed.Freshens[0].Id

	// 5. `hotbed freshen show` by command ID and by base ID + --node.
	var shown pb.GetFreshenResponse
	c.ok(&shown, "freshen", "show", g2CmdID)
	if shown.Freshen == nil || shown.Freshen.Id != g2CmdID || shown.Freshen.FailedStep != "broken-step" {
		t.Fatalf("freshen show %s = %+v", g2CmdID, &shown)
	}
	if shown.Freshen.FromBaseId != "app-g1" {
		t.Errorf("freshen g2 FromBaseId = %q, want app-g1", shown.Freshen.FromBaseId)
	}

	// Base ID app-g1 exists on both nodes -> conflict without --node, succeeds with --node.
	if _, e := c.fails("freshen", "show", "app-g1"); e.Code != api.CodeConflict {
		t.Fatalf("freshen show ambiguous app-g1 = %+v, want CONFLICT", e)
	}
	var shownG1 pb.GetFreshenResponse
	c.ok(&shownG1, "freshen", "show", "app-g1", "--node", "asahimbp")
	if shownG1.Freshen == nil || shownG1.Freshen.ColdReason != "no_previous_base" {
		t.Fatalf("freshen show app-g1 --node asahimbp = %+v, want cold_reason=no_previous_base", shownG1.Freshen)
	}

	code, showPlain, stderr := c.hotbed("freshen", "show", g2CmdID)
	if code != exitOK {
		t.Fatalf("freshen show plain exited %d: %s", code, stderr)
	}
	for _, want := range []string{"Freshen " + g2CmdID, "Timings & resources:", "Steps:", "broken-step"} {
		if !strings.Contains(showPlain, want) {
			t.Errorf("freshen show plain missing %q:\n%s", want, showPlain)
		}
	}

	// 6. MCP tools `freshen_ls` and `freshen_show`.
	t.Setenv("T3CODE_THREAD_ID", "thread-freshen-mcp")
	m := c.mcp()
	defer m.stop()

	mcpLs := &pb.ListFreshensResponse{}
	m.call(mcpLs, "freshen_ls", map[string]any{"project": "app", "cold": true})
	if len(mcpLs.Freshens) < 3 {
		t.Fatalf("MCP freshen_ls(cold=true) = %d, want >= 3", len(mcpLs.Freshens))
	}
	mcpShow := &pb.GetFreshenResponse{}
	m.call(mcpShow, "freshen_show", map[string]any{"id": g2CmdID})
	if mcpShow.Freshen == nil || mcpShow.Freshen.BaseId != "app-g2" {
		t.Fatalf("MCP freshen_show = %+v", mcpShow)
	}
}
