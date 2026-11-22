package controller

import (
	"slices"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
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
}
