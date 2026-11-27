package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcpskills "github.com/modelcontextprotocol/go-sdk/extensions/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/skillspec"
	"github.com/semistrict/hotbed/skills"
)

func TestSkillsLifecycleSessionRecallAndNpxAdd(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "brave-otter")
	c.ok(nil, "session", "start", "other-fox", "-p", "other")

	// Create a local directory with two skills (one multi-file with references + binary file).
	srcDir := t.TempDir()
	writeSrc := func(rel string, data []byte) {
		p := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSrc("skills/code-review/SKILL.md", []byte("---\nname: code-review\ndescription: Review code changes for bugs and security issues.\nlicense: MIT\ncompatibility: Works everywhere\n---\n\n# Code Review\nRead `references/checklist.md`.\n"))
	writeSrc("skills/code-review/references/checklist.md", []byte("# Checklist\n- Check races\n- Check error paths\n"))
	writeSrc("skills/code-review/assets/logo.bin", []byte{0x00, 0x01, 0x02, 0xff})
	writeSrc("skills/deploy-app/SKILL.md", []byte("---\nname: deploy-app\ndescription: Deploy the app safely.\n---\n\n# Deploy\nRun deploy.\n"))

	// 1. Preview with --list (npx skills add <src> --list) installs nothing.
	var preview skillsAddResult
	c.ok(&preview, "skills", "add", srcDir, "--list")
	if len(preview.Listed) != 2 || len(preview.Installed) != 0 {
		t.Fatalf("preview = %+v, want 2 listed and 0 installed", preview)
	}

	// 2. Install code-review globally using -s (npx skills style).
	var addedGlobal skillsAddResult
	c.ok(&addedGlobal, "skills", "add", srcDir, "--global", "-s", "code-review", "--tag", "review", "--rationale", "team standard")
	if len(addedGlobal.Installed) != 1 {
		t.Fatalf("addedGlobal = %+v, want 1 installed", addedGlobal)
	}
	sk1 := addedGlobal.Installed[0]
	if sk1.Id != 1 || sk1.Name != "code-review" || sk1.Scope != pb.SkillScope_SKILL_SCOPE_GLOBAL || sk1.FileCount != 3 || sk1.Uri != "skill://global/code-review/SKILL.md" {
		t.Fatalf("sk1 = %+v", sk1)
	}
	if !strings.HasSuffix(sk1.CockpitUrl, "/skills/1") {
		t.Errorf("sk1 cockpitUrl = %q, want suffix /skills/1", sk1.CockpitUrl)
	}

	// 3. Install deploy-app scoped to the active session's project ("test") with --rename.
	var addedProj skillsAddResult
	c.ok(&addedProj, "skills", "add", srcDir, "--session", "brave-otter", "-s", "deploy-app", "--rename", "release-prod")
	if len(addedProj.Installed) != 1 {
		t.Fatalf("addedProj = %+v, want 1 installed", addedProj)
	}
	sk2 := addedProj.Installed[0]
	if sk2.Name != "release-prod" || sk2.Project != "test" || sk2.Uri != "skill://test/release-prod/SKILL.md" {
		t.Fatalf("sk2 = %+v", sk2)
	}
	if sk2.Source == nil || sk2.Source.OriginalName != "deploy-app" {
		t.Fatalf("sk2.Source = %+v, want original_name=deploy-app", sk2.Source)
	}

	// 4. Install a skill scoped to "other" project so we can verify project isolation.
	var addedOther skillsAddResult
	c.ok(&addedOther, "skills", "add", srcDir, "-p", "other", "-s", "deploy-app")
	if len(addedOther.Installed) != 1 || addedOther.Installed[0].Project != "other" {
		t.Fatalf("addedOther = %+v", addedOther)
	}

	// 5. Session start in "test" outputs ALL relevant skill descriptors: the
	// CLI's built-in ones first, then the stored ones (global + test, NOT other).
	started := &pb.Session{}
	c.ok(started, "session", "start", "keen-fox")
	builtins, err := skills.List()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, b := range builtins {
		want = append(want, "builtin "+b.Name)
	}
	want = append(want, "test release-prod", "global code-review")
	for _, d := range started.GetSkills().GetSkills() {
		got = append(got, skillScopeTag(d)+" "+d.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("session start skills = %q, want %q", got, want)
	}
	if started.Skills.TotalActive != 2 {
		t.Errorf("session start total active stored skills = %d, want 2", started.Skills.TotalActive)
	}

	// Plain-text session start prints skill descriptors cleanly.
	var plainOut, plainErr bytes.Buffer
	plainDeps := c.cli
	plainDeps.stdout, plainDeps.stderr = &plainOut, &plainErr
	if code := run(t.Context(), []string{"session", "start", "wise-owl"}, plainDeps); code != exitOK {
		t.Fatalf("plain session start failed (%d): %s", code, plainErr.String())
	}
	outStr := plainOut.String()
	wantStored := "\n  [test] release-prod — Deploy the app safely.\n  [global] code-review — Review code changes for bugs and security issues.\n"
	if !strings.HasPrefix(outStr, "wise-owl  project=test\n\nSkills (read one with `hotbed skills get NAME` when its description applies):\n  [builtin] hotbed — ") ||
		!strings.Contains(outStr, wantStored) {
		t.Errorf("plain session start = %q, want the built-in skills first, then%q", outStr, wantStored)
	}

	// --omit-skills suppresses skill descriptors on session start.
	omitted := &pb.Session{}
	c.ok(omitted, "session", "start", "quiet-seal", "--omit-skills")
	if omitted.Skills != nil {
		t.Errorf("expected nil skills with --omit-skills, got %+v", omitted.Skills)
	}

	// 6. List, Get (SKILL.md + nested file + --all-files + built-in hotbed), Show.
	var listed skillsListResult
	c.ok(&listed, "skills", "ls", "-s", "brave-otter")
	if len(listed.Skills) != 2 {
		t.Fatalf("skills ls = %d, want 2", len(listed.Skills))
	}

	var gotMD skillsGetResult
	c.ok(&gotMD, "skills", "get", "code-review")
	if !strings.Contains(gotMD.Content, "# Code Review") {
		t.Errorf("skills get code-review content = %q", gotMD.Content)
	}
	var gotRef skillsGetResult
	c.ok(&gotRef, "skills", "get", "code-review", "references/checklist.md")
	if !strings.Contains(gotRef.Content, "Check races") {
		t.Errorf("skills get reference = %q", gotRef.Content)
	}
	var gotAll skillsGetResult
	c.ok(&gotAll, "skills", "get", "code-review", "--all-files")
	if len(gotAll.Files) != 3 {
		t.Errorf("skills get --all-files = %d files, want 3", len(gotAll.Files))
	}
	var gotBuiltin skillsGetResult
	c.ok(&gotBuiltin, "skills", "get", "hotbed")
	if !strings.Contains(gotBuiltin.Content, "hotbed session start") {
		t.Errorf("skills get hotbed missing expected content")
	}
	// Every built-in skill reads by its name, and `skills ls -q` matches
	// built-ins as it matches stored ones.
	var gotQuestions skillsGetResult
	c.ok(&gotQuestions, "skills", "get", "hotbed-questions")
	if !strings.HasPrefix(gotQuestions.Content, "---\nname: hotbed-questions\n") {
		t.Errorf("skills get hotbed-questions = %q, want the built-in skill", apicmd.Short(120, gotQuestions.Content))
	}
	for _, args := range [][]string{
		{"skills", "ls", "-s", "brave-otter", "-q", "cron schedule"},
		{"skills", "ls", "--builtin", "-q", "cron schedule"},
	} {
		var matched skillsListResult
		c.ok(&matched, args...)
		if len(matched.Builtin) != 1 || matched.Builtin[0].Name != "hotbed-schedules" {
			t.Errorf("%q built-ins = %+v, want only hotbed-schedules", args, matched.Builtin)
		}
	}

	shown := &pb.Skill{}
	c.ok(shown, "skills", "show", "code-review")
	if shown.Id != sk1.Id || len(shown.Files) != 3 || len(shown.History) == 0 {
		t.Fatalf("skills show = %+v", shown)
	}

	// 7. Edit, Move, Promote, Archive, Restore, Delete (exercises every SkillService RPC).
	edited := &pb.Skill{}
	c.ok(edited, "skills", "edit", "code-review", "--tag", "security", "--rationale", "updated rationale")
	if !slices.Contains(edited.Tags, "security") {
		t.Errorf("edited tags = %v, want security", edited.Tags)
	}

	moved := &pb.Skill{}
	c.ok(moved, "skills", "move", "code-review", "--first")
	if moved.Id != sk1.Id {
		t.Errorf("moved = %+v", moved)
	}

	promoted := &pb.Skill{}
	c.ok(promoted, "skills", "promote", "release-prod", "-s", "brave-otter")
	if promoted.Scope != pb.SkillScope_SKILL_SCOPE_GLOBAL || promoted.Uri != "skill://global/release-prod/SKILL.md" {
		t.Errorf("promoted = %+v", promoted)
	}

	archived := &pb.Skill{}
	c.ok(archived, "skills", "archive", "release-prod", "--reason", "testing archive")
	if archived.State != pb.SkillState_SKILL_STATE_ARCHIVED {
		t.Errorf("archived state = %v", archived.State)
	}

	// Archived skill no longer appears on session start.
	afterArch := &pb.Session{}
	c.ok(afterArch, "session", "start", "calm-lynx")
	if got := storedSkillNames(afterArch); !slices.Equal(got, []string{"code-review"}) {
		t.Errorf("after archive session stored skills = %q, want only code-review", got)
	}

	restored := &pb.Skill{}
	c.ok(restored, "skills", "restore", "2")
	if restored.State != pb.SkillState_SKILL_STATE_ACTIVE {
		t.Errorf("restored state = %v", restored.State)
	}

	// 8. Check & Update from upstream local source (detects edit to SKILL.md and re-syncs renamed skill!).
	writeSrc("skills/deploy-app/SKILL.md", []byte("---\nname: deploy-app\ndescription: Deploy the app safely (v2).\n---\n\n# Deploy v2\nUpdated instructions.\n"))
	var checked skillsCheckResult
	c.ok(&checked, "skills", "check", "release-prod")
	if checked.UpdatesAvailable != 1 || len(checked.Skills) != 1 || checked.Skills[0].Status != "update-available" {
		t.Fatalf("skills check = %+v, want 1 update-available", checked)
	}

	var updated skillsCheckResult
	c.ok(&updated, "skills", "update", "release-prod")
	if updated.Updated != 1 || len(updated.Skills) != 1 || updated.Skills[0].Status != "updated" {
		t.Fatalf("skills update = %+v, want 1 updated", updated)
	}
	var gotUpdated skillsGetResult
	c.ok(&gotUpdated, "skills", "get", "release-prod")
	if !strings.Contains(gotUpdated.Content, "name: release-prod") || !strings.Contains(gotUpdated.Content, "# Deploy v2") {
		t.Errorf("updated renamed skill content = %q", gotUpdated.Content)
	}

	// 9. Remove skill permanently.
	removed := &pb.Skill{}
	c.ok(removed, "skills", "rm", "release-prod")
	if removed.Id != sk2.Id {
		t.Errorf("removed = %+v", removed)
	}
	if _, e := c.fails("skills", "show", "release-prod"); e.Code != api.CodeNotFound {
		t.Errorf("show after rm = %v, want NOT_FOUND", e)
	}
}

func TestSkillsGitSourceFindInitAndLocalInstall(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.ok(nil, "session", "start", "git-otter")

	// Build a local git repository containing a skill to exercise real git clone + commit pinning.
	repoDir := t.TempDir()
	skillDir := filepath.Join(repoDir, "skills", "git-demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: git-demo\ndescription: Skill installed from git.\n---\n# Git Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "initial skill"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	var added skillsAddResult
	c.ok(&added, "skills", "add", "file://"+repoDir, "--global")
	if len(added.Installed) != 1 || added.Installed[0].Name != "git-demo" || added.Installed[0].Source == nil || len(added.Installed[0].Source.Commit) != 40 {
		t.Fatalf("git add result = %+v", added)
	}

	// `hotbed skills install --from-hotbed -a claude-code` materializes Hotbed skills locally.
	workDir := t.TempDir()
	c.cli.getwd = func() (string, error) { return workDir, nil }
	var synced skillsLocalSyncResult
	c.ok(&synced, "skills", "install", "--from-hotbed", "-a", "claude-code")
	if len(synced.Installed) != 1 || synced.Installed[0].SkillName != "git-demo" {
		t.Fatalf("local sync = %+v", synced)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "git-demo", "SKILL.md")); err != nil {
		t.Fatalf("missing installed claude skill: %v", err)
	}

	// `hotbed skills init` scaffolds a starter SKILL.md.
	var inited skillsInitResult
	c.ok(&inited, "skills", "init", "starter-skill")
	if inited.Name != "starter-skill" {
		t.Errorf("init = %+v", inited)
	}
	if _, err := os.Stat(filepath.Join(workDir, "starter-skill", "SKILL.md")); err != nil {
		t.Fatalf("missing init SKILL.md: %v", err)
	}

	// `hotbed skills find` queries SKILLS_API_URL.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" && r.URL.Query().Get("q") == "react" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"skills": []map[string]any{{
					"id": "vercel-labs/agent-skills/react-best-practices", "name": "react-best-practices", "source": "vercel-labs/agent-skills", "installs": 12000,
				}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer apiSrv.Close()
	c.setenv("SKILLS_API_URL", apiSrv.URL)

	var found skillsFindResult
	c.ok(&found, "skills", "find", "react")
	if len(found.Skills) != 1 || found.Skills[0].Name != "react-best-practices" {
		t.Fatalf("find = %+v", found)
	}
}

func TestMCPSkillsExtensionSEP2640AndResources(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.ok(nil, "project", "add", "other")
	c.ok(nil, "session", "start", "seed-otter")

	srcDir := t.TempDir()
	mustWrite := func(rel string, data []byte) {
		p := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("skills/pdf-processing/SKILL.md", []byte("---\nname: pdf-processing\ndescription: Extract, fill, and assemble PDF documents.\nlicense: Apache-2.0\n---\n\n# PDF Processing\nPick a template from `templates/`.\n"))
	mustWrite("skills/pdf-processing/templates/invoice.md", []byte("# Invoice\nCustomer:\n"))
	mustWrite("skills/pdf-processing/templates/nested/regional.md", []byte("# Regional\n"))
	mustWrite("skills/pdf-processing/assets/icon.bin", []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01})
	mustWrite("skills/proj-only/SKILL.md", []byte("---\nname: proj-only\ndescription: Only for project test.\n---\n# Test\n"))
	mustWrite("skills/other-only/SKILL.md", []byte("---\nname: other-only\ndescription: Only for project other.\n---\n# Other\n"))

	c.ok(nil, "skills", "add", srcDir, "--global", "-s", "pdf-processing")
	c.ok(nil, "skills", "add", srcDir, "-p", "test", "-s", "proj-only")
	c.ok(nil, "skills", "add", srcDir, "-p", "other", "-s", "other-only")

	// Start hotbed mcp with a SEP-2640-enabled MCP client.
	c.setenv("T3CODE_THREAD_ID", "thread-mcp-skills")
	serverIn, clientOut := osPipeOrIOPipe(t)
	clientIn, serverOut := osPipeOrIOPipe(t)
	var stderr bytes.Buffer
	d := c.cli
	d.stdin, d.stdout, d.stderr = serverIn, serverOut, &stderr
	done := make(chan int, 1)
	go func() {
		code := run(t.Context(), []string{"mcp"}, d)
		serverOut.Close()
		done <- code
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "sep2640-client", Version: "1.0.0"}, nil)
	if err := mcpskills.AddMethods(client); err != nil {
		t.Fatalf("mcpskills.AddMethods: %v", err)
	}
	sess, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatalf("connect: %v (stderr: %s)", err, stderr.String())
	}
	defer func() {
		sess.Close()
		clientOut.Close()
		if code := <-done; code != exitOK {
			t.Errorf("hotbed mcp exited %d: %s", code, stderr.String())
		}
	}()

	// 1. Capabilities declare io.modelcontextprotocol/skills with directoryRead: true + resources.
	initRes := sess.InitializeResult()
	if initRes.Capabilities == nil || initRes.Capabilities.Resources == nil {
		t.Fatalf("missing resources capability: %+v", initRes.Capabilities)
	}
	extRaw, ok := initRes.Capabilities.Extensions[mcpskills.ExtensionID]
	if !ok {
		t.Fatalf("missing %s extension in capabilities: %+v", mcpskills.ExtensionID, initRes.Capabilities.Extensions)
	}
	extMap, _ := extRaw.(map[string]any)
	if extMap["directoryRead"] != true {
		t.Errorf("extension settings = %#v, want directoryRead=true", extRaw)
	}

	// 2. Instructions list each relevant skill with the URI to read it by
	// (and NOT other-only): a scope tag such as [test/REPO] makes no URI.
	for _, want := range []string{
		"\n  [test] proj-only — Only for project test. (skill://test/proj-only/SKILL.md)",
		"\n  [global] pdf-processing — Extract, fill, and assemble PDF documents. (skill://global/pdf-processing/SKILL.md)",
		" (skill://builtin/hotbed/SKILL.md)",
	} {
		if !strings.Contains(initRes.Instructions, want) {
			t.Errorf("instructions missing %q:\n%s", want, initRes.Instructions)
		}
	}
	if strings.Contains(initRes.Instructions, "other-only") {
		t.Errorf("instructions leaked another project's skill:\n%s", initRes.Instructions)
	}

	// 3. SEP-2640 skills/list returns global + test + builtin/hotbed (and NOT other-only).
	listedURIs := map[string]*mcpskills.Skill{}
	for sk, err := range mcpskills.List(t.Context(), sess, nil, nil) {
		if err != nil {
			t.Fatalf("skills/list: %v", err)
		}
		listedURIs[sk.URI] = sk
	}
	for _, want := range []string{
		"skill://global/pdf-processing/SKILL.md",
		"skill://test/proj-only/SKILL.md",
		"skill://builtin/hotbed/SKILL.md",
	} {
		if listedURIs[want] == nil {
			t.Errorf("skills/list missing %s (got %v)", want, listedURIs)
		}
	}
	if listedURIs["skill://other/other-only/SKILL.md"] != nil {
		t.Errorf("skills/list leaked other project's skill")
	}

	// 4. SEP-2640 skills/get + resources/read + cryptographic VerifySkillMD & VerifyResource.
	pdfRes, err := mcpskills.Get(t.Context(), sess, &mcpskills.GetSkillParams{URI: "skill://global/pdf-processing/SKILL.md"})
	if err != nil {
		t.Fatalf("skills/get pdf-processing: %v", err)
	}
	if pdfRes.Skill.Frontmatter["license"] != "Apache-2.0" {
		t.Errorf("frontmatter license = %v, want Apache-2.0", pdfRes.Skill.Frontmatter["license"])
	}
	mdRead, err := sess.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: pdfRes.Skill.URI})
	if err != nil {
		t.Fatalf("resources/read SKILL.md: %v", err)
	}
	if err := mcpskills.VerifySkillMD(pdfRes.Skill, []byte(mdRead.Contents[0].Text)); err != nil {
		t.Fatalf("VerifySkillMD failed: %v", err)
	}
	invRead, err := sess.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "skill://global/pdf-processing/templates/invoice.md"})
	if err != nil {
		t.Fatalf("resources/read invoice.md: %v", err)
	}
	if err := mcpskills.VerifyResource(pdfRes.Skill, "skill://global/pdf-processing/templates/invoice.md", []byte(invRead.Contents[0].Text)); err != nil {
		t.Fatalf("VerifyResource invoice.md failed: %v", err)
	}
	binRead, err := sess.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "skill://global/pdf-processing/assets/icon.bin"})
	if err != nil {
		t.Fatalf("resources/read icon.bin: %v", err)
	}
	if len(binRead.Contents[0].Blob) != 6 {
		t.Fatalf("expected binary blob of 6 bytes, got %+v", binRead.Contents[0])
	}
	if err := mcpskills.VerifyResource(pdfRes.Skill, "skill://global/pdf-processing/assets/icon.bin", binRead.Contents[0].Blob); err != nil {
		t.Fatalf("VerifyResource icon.bin failed: %v", err)
	}

	// Built-in hotbed skill also verifies end-to-end via SEP-2640!
	hbRes, err := mcpskills.Get(t.Context(), sess, &mcpskills.GetSkillParams{URI: "skill://builtin/hotbed/SKILL.md"})
	if err != nil {
		t.Fatalf("skills/get builtin hotbed: %v", err)
	}
	hbRead, err := sess.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: hbRes.Skill.URI})
	if err != nil {
		t.Fatalf("resources/read builtin hotbed: %v", err)
	}
	if err := mcpskills.VerifySkillMD(hbRes.Skill, []byte(hbRead.Contents[0].Text)); err != nil {
		t.Fatalf("VerifySkillMD builtin hotbed failed: %v", err)
	}

	// Unknown skill returns JSON-RPC Invalid Params (-32602).
	if _, err := mcpskills.Get(t.Context(), sess, &mcpskills.GetSkillParams{URI: "skill://global/does-not-exist/SKILL.md"}); err == nil {
		t.Errorf("expected error for unknown skill URI")
	}

	// 5. SEP-2640 resources/directory/read on skill root and nested subdir.
	rootDir, err := mcpskills.ReadDirectory(t.Context(), sess, &mcpskills.ReadDirectoryParams{URI: "skill://global/pdf-processing"})
	if err != nil {
		t.Fatalf("resources/directory/read root: %v", err)
	}
	childTypes := map[string]string{}
	for _, r := range rootDir.Resources {
		childTypes[r.Name] = r.MIMEType
	}
	if childTypes["SKILL.md"] != "text/markdown" || childTypes["templates"] != "inode/directory" || childTypes["assets"] != "inode/directory" {
		t.Errorf("root directory children = %v", childTypes)
	}
	subDir, err := mcpskills.ReadDirectory(t.Context(), sess, &mcpskills.ReadDirectoryParams{URI: "skill://global/pdf-processing/templates"})
	if err != nil {
		t.Fatalf("resources/directory/read templates: %v", err)
	}
	subTypes := map[string]string{}
	for _, r := range subDir.Resources {
		subTypes[r.Name] = r.MIMEType
	}
	if subTypes["invoice.md"] != "text/markdown" || subTypes["nested"] != "inode/directory" {
		t.Errorf("templates directory children = %v", subTypes)
	}

	// 6. Standard MCP resources/list also exposes top-level SKILL.md resources for pre-SEP-2640 clients.
	resList, err := sess.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	var stdURIs []string
	for _, r := range resList.Resources {
		stdURIs = append(stdURIs, r.URI)
	}
	if !slices.Contains(stdURIs, "skill://global/pdf-processing/SKILL.md") || !slices.Contains(stdURIs, "skill://builtin/hotbed/SKILL.md") {
		t.Errorf("resources/list = %v, missing expected SKILL.md URIs", stdURIs)
	}
	_ = skillspec.MaxFilesPerSkill
}

func osPipeOrIOPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		w.Close()
	})
	return r, w
}

// storedSkillNames names the controller's skills a session start briefed,
// leaving out the CLI's built-in ones.
func storedSkillNames(sess *pb.Session) []string {
	var out []string
	for _, d := range sess.GetSkills().GetSkills() {
		if d.Scope != pb.SkillScope_SKILL_SCOPE_BUILTIN {
			out = append(out, d.Name)
		}
	}
	return out
}
