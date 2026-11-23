package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/tailnet"
)

func TestPutRepoAlwaysBelongsToAProject(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{Store: store, Now: func() time.Time { return now }})

	// A project called "app" already holds a different repo.
	if _, err := store.AddProject(ctx, "app", []string{"github.com/first/app"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// Adding another repo whose name is also "app" gets a unique project name
	// instead of silently remaining projectless.
	if _, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
		Repo: &pb.Repo{Id: "app", Url: "https://github.com/second/app.git"},
	}); err != nil {
		t.Fatal(err)
	}
	holding, err := store.ProjectsHolding(ctx, "github.com/second/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(holding) != 1 || holding[0].Name != "app-2" {
		t.Fatalf("ProjectsHolding(second/app) = %v, want [app-2]", holding)
	}

	// PutRepo with an explicit project requires the project to exist and places
	// the repo directly into it.
	if _, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
		Repo:    &pb.Repo{Id: "api", Url: "https://github.com/second/api.git"},
		Project: "suite",
	}); err == nil {
		t.Fatal("PutRepo with unknown project succeeded; want NOT_FOUND")
	}
	if _, err := store.AddProject(ctx, "suite", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
		Repo:    &pb.Repo{Id: "api", Url: "https://github.com/second/api.git"},
		Project: "suite",
	}); err != nil {
		t.Fatal(err)
	}
	suite, err := store.Project(ctx, "suite")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(suite.Repos, []string{"github.com/second/api"}) {
		t.Errorf("suite repos = %v, want [github.com/second/api]", suite.Repos)
	}
	listed, err := srv.ListRepos(ctx, &pb.ListReposRequest{Project: "suite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Repos) != 1 || listed.Repos[0].Id != "api" {
		t.Errorf("ListRepos(suite) = %v, want [api]", listed.Repos)
	}
	if listed.Repos[0].Reminder != nil {
		t.Errorf("ListRepos(suite) persisted reminder: %q", listed.Repos[0].GetReminder())
	}
}

func TestListBases(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{Store: store, Now: func() time.Time { return now }})

	// Seed two projects, a repo under a differently named project, and an empty project.
	if _, err := store.AddProject(ctx, "suite", []string{"github.com/acme/api"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddProject(ctx, "other", []string{"github.com/acme/other"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddProject(ctx, "empty-proj", nil, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRepo(ctx, &pb.Repo{Id: "api-repo", Url: "https://github.com/acme/api.git"}); err != nil {
		t.Fatal(err)
	}

	// Join two nodes and push bases with multiple generations, states, and a failed step.
	for _, n := range []struct{ id, host string }{{"n-b", "ubu1"}, {"n-a", "asahimbp"}} {
		if _, err := store.Join(ctx, tailnet.Peer{NodeID: n.id, Hostname: n.host}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Push(ctx, "n-a", &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Bases: []*pb.Base{
				{
					Id: "suite-g1", Project: "suite", NodeId: "n-a", Generation: 1,
					State: pb.BaseState_BASE_STATE_STALE, Command: "cmd-a1",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "1111111abcdef"}},
				},
				{
					Id: "suite-g3", Project: "suite", NodeId: "n-a", Generation: 3,
					State: pb.BaseState_BASE_STATE_FAILED, Command: "cmd-a3",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "3333333abcdef"}},
					Steps: []*pb.AppliedStep{
						{Name: "deps", Ran: true, Ok: true},
						{Name: "cargo-build", Ran: true, Ok: false},
					},
				},
				{
					Id: "suite-g2", Project: "suite", NodeId: "n-a", Generation: 2,
					State: pb.BaseState_BASE_STATE_WARM, Command: "cmd-a2",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "2222222abcdef"}},
				},
			},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Push(ctx, "n-b", &pb.PushEventsRequest{
		Epoch: "e1",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Bases: []*pb.Base{
				{
					Id: "other-g1", Project: "other", NodeId: "n-b", Generation: 1,
					State: pb.BaseState_BASE_STATE_BUILDING, Command: "cmd-b1",
				},
				{
					Id: "suite-g2", Project: "suite", NodeId: "n-b", Generation: 2,
					State: pb.BaseState_BASE_STATE_WARM, Command: "cmd-b2",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "2222222abcdef"}},
				},
			},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}

	// 1. All projects: sorted by project asc, node asc, generation desc.
	all, err := srv.ListBases(ctx, &pb.ListBasesRequest{})
	if err != nil {
		t.Fatalf("ListBases(all): %v", err)
	}
	var gotOrder []string
	for _, nb := range all.Bases {
		gotOrder = append(gotOrder, nb.Base.GetProject()+"/"+nb.Node+"/"+nb.Base.GetId())
	}
	wantOrder := []string{
		"other/ubu1/other-g1",
		"suite/asahimbp/suite-g3",
		"suite/asahimbp/suite-g2",
		"suite/asahimbp/suite-g1",
		"suite/ubu1/suite-g2",
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("ListBases(all) order = %v, want %v", gotOrder, wantOrder)
	}
	// Failed base preserves freshen command ID and failed step name.
	failed := all.Bases[1]
	if failed.Base.Command != "cmd-a3" || failed.FailedStep != "cargo-build" {
		t.Errorf("failed base = command:%q failedStep:%q, want cmd-a3 / cargo-build", failed.Base.Command, failed.FailedStep)
	}

	// 2. Filtered by project name.
	byProj, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "suite"})
	if err != nil {
		t.Fatalf("ListBases(suite): %v", err)
	}
	if len(byProj.Bases) != 4 {
		t.Fatalf("ListBases(suite) = %d bases, want 4", len(byProj.Bases))
	}

	// 3. Filtered by repo ID resolves to the project holding it.
	byRepo, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "api-repo"})
	if err != nil {
		t.Fatalf("ListBases(api-repo): %v", err)
	}
	if len(byRepo.Bases) != 4 {
		t.Fatalf("ListBases(api-repo) = %d bases, want 4", len(byRepo.Bases))
	}

	// 4. Filtered by node hostname and by state.
	byNode, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "suite", Node: "asahimbp", State: pb.BaseState_BASE_STATE_FAILED})
	if err != nil {
		t.Fatalf("ListBases(suite, asahimbp, failed): %v", err)
	}
	if len(byNode.Bases) != 1 || byNode.Bases[0].Base.Id != "suite-g3" || byNode.Bases[0].FailedStep != "cargo-build" {
		t.Fatalf("ListBases(suite, asahimbp, failed) = %+v, want [suite-g3]", byNode.Bases)
	}

	// 5. Known project with zero bases returns empty list without error.
	empty, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "empty-proj"})
	if err != nil || len(empty.Bases) != 0 {
		t.Fatalf("ListBases(empty-proj) = (%v, %v), want empty nil err", empty, err)
	}

	// 6. Unknown project returns NOT_FOUND with suggestion.
	if _, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "no-such-project"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("ListBases(unknown project) err = %v (code %s), want NOT_FOUND", err, api.CodeOf(err))
	} else if !strings.Contains(err.Error(), "no-such-project") {
		t.Errorf("unknown project error = %q, want project name", err.Error())
	}

	// 7. Unknown node returns NOT_FOUND.
	if _, err := srv.ListBases(ctx, &pb.ListBasesRequest{Node: "ghost-node"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("ListBases(unknown node) err = %v (code %s), want NOT_FOUND", err, api.CodeOf(err))
	}

	// 8. Invalid state enum returns INVALID_ARGS.
	if _, err := srv.ListBases(ctx, &pb.ListBasesRequest{State: pb.BaseState(99)}); api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("ListBases(invalid state) err = %v (code %s), want INVALID_ARGS", err, api.CodeOf(err))
	}

	// 9. Repo shared by two projects returns bases from both projects.
	if _, err := store.AddProject(ctx, "suite-extra", []string{"github.com/acme/api"}, &pb.Stamp{At: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Push(ctx, "n-b", &pb.PushEventsRequest{
		Epoch: "e2",
		Checkpoint: &pb.Checkpoint{
			Seq: 1,
			Bases: []*pb.Base{
				{
					Id: "other-g1", Project: "other", NodeId: "n-b", Generation: 1,
					State: pb.BaseState_BASE_STATE_BUILDING, Command: "cmd-b1",
				},
				{
					Id: "suite-g2", Project: "suite", NodeId: "n-b", Generation: 2,
					State: pb.BaseState_BASE_STATE_WARM, Command: "cmd-b2",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "2222222abcdef"}},
				},
				{
					Id: "suite-extra-g1", Project: "suite-extra", NodeId: "n-b", Generation: 1,
					State: pb.BaseState_BASE_STATE_WARM, Command: "cmd-b3",
					Repos: []*pb.BaseRepo{{Repo: "api-repo", Commit: "2222222abcdef"}},
				},
				{
					// Base whose project record has been removed (never existed in projects table).
					Id: "retired-g1", Project: "retired-proj", NodeId: "n-b", Generation: 1,
					State: pb.BaseState_BASE_STATE_STALE, Command: "cmd-b4",
				},
			},
		},
	}, now, nil); err != nil {
		t.Fatal(err)
	}
	shared, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "api-repo"})
	if err != nil {
		t.Fatalf("ListBases(shared api-repo): %v", err)
	}
	var sharedOrder []string
	for _, nb := range shared.Bases {
		sharedOrder = append(sharedOrder, nb.Base.GetProject()+"/"+nb.Node+"/"+nb.Base.GetId())
	}
	wantShared := []string{
		"suite/asahimbp/suite-g3",
		"suite/asahimbp/suite-g2",
		"suite/asahimbp/suite-g1",
		"suite/ubu1/suite-g2",
		"suite-extra/ubu1/suite-extra-g1",
	}
	if !slices.Equal(sharedOrder, wantShared) {
		t.Fatalf("ListBases(shared api-repo) = %v, want %v", sharedOrder, wantShared)
	}

	// 10. Base whose project record was removed is still matched by project name.
	orphan, err := srv.ListBases(ctx, &pb.ListBasesRequest{Project: "retired-proj"})
	if err != nil {
		t.Fatalf("ListBases(retired-proj): %v", err)
	}
	if len(orphan.Bases) != 1 || orphan.Bases[0].Base.GetId() != "retired-g1" || orphan.Bases[0].Node != "ubu1" {
		t.Fatalf("ListBases(retired-proj) = %+v, want [retired-g1 on ubu1]", orphan.Bases)
	}
}

func TestGetProfileTargetValidation(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{Store: store, Now: func() time.Time { return now }})
	ws := &workspaces{Server: srv}

	for _, tc := range []struct {
		name string
		req  *pb.GetProfileRequest
		code api.Code
	}{
		{
			name: "nil target",
			req:  &pb.GetProfileRequest{},
			code: api.CodeInvalidArgs,
		},
		{
			name: "blank command_id",
			req:  &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: "   "}},
			code: api.CodeInvalidArgs,
		},
		{
			name: "whitespace-padded command_id",
			req:  &pb.GetProfileRequest{Target: &pb.GetProfileRequest_CommandId{CommandId: " cmd1 "}},
			code: api.CodeInvalidArgs,
		},
		{
			name: "blank node",
			req:  &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "   "}},
			code: api.CodeInvalidArgs,
		},
		{
			name: "unknown node",
			req:  &pb.GetProfileRequest{Target: &pb.GetProfileRequest_Node{Node: "ghost-node"}},
			code: api.CodeNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ws.GetProfile(ctx, tc.req); api.CodeOf(err) != tc.code {
				t.Fatalf("GetProfile(%s) err = %v (code %s), want %s", tc.name, err, api.CodeOf(err), tc.code)
			}
		})
	}
}
