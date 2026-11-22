package controller

import (
	"slices"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestProjectNameFor(t *testing.T) {
	for remote, want := range map[string]string{
		"github.com/semistrict/hotbed": "hotbed",
		"github.com/vercel/Next.js":    "next-js",
		"github.com/o/my_repo":         "my-repo",
		"github.com/o/MyApp":           "myapp",
		"github.com/o/dotfiles.nvim":   "dotfiles-nvim",
		"github.com/o/__":              "",
		"github.com/o/" + "a234567890b234567890c234567890d234567890-e": "a234567890b234567890c234567890d234567890",
		"/srv/git/Tools-": "tools",
	} {
		if got := projectNameFor(remote); got != want {
			t.Errorf("projectNameFor(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestEnsureProjectsCreatesRepoProjectsAndAttachesOrphanSessions(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{Store: store, Now: func() time.Time { return now }})

	// Simulate state from before projects existed: a registered repo and a
	// session with NULL project_id holding a workspace of that repo.
	if err := store.PutRepo(ctx, &pb.Repo{Id: "cladefs", Url: "https://github.com/semistrict/cladefs.git", CreatedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions (id, agent, started_at, last_active_at) VALUES (?, ?, ?, ?)`,
		"accurate-gazelle", "laptop", now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := store.AddWorkspace(ctx, "ws-1", "accurate-gazelle", "n1", "cladefs", "laptop", now); err != nil {
		t.Fatal(err)
	}

	if err := srv.EnsureProjects(ctx); err != nil {
		t.Fatal(err)
	}

	p, err := store.Project(ctx, "cladefs")
	if err != nil {
		t.Fatalf("expected auto-created project cladefs: %v", err)
	}
	if !slices.Equal(p.Repos, []string{"github.com/semistrict/cladefs"}) {
		t.Errorf("project cladefs repos = %v, want [github.com/semistrict/cladefs]", p.Repos)
	}
	sess, err := store.Session(ctx, "accurate-gazelle")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != "cladefs" {
		t.Errorf("orphan session project = %q, want cladefs", sess.Project)
	}
}
