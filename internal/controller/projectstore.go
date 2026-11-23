package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// AddProject makes the project called name, unless it is there already,
// and adds remotes (canonical) to the repos it holds, recording each change
// as by's.
func (s *Store) AddProject(ctx context.Context, name string, remotes []string, by *pb.Stamp) (*pb.Project, error) {
	p, _, err := s.addProject(ctx, name, remotes, by)
	return p, err
}

// addProject is AddProject, also reporting whether the project was created or
// any repo was newly added to it.
func (s *Store) addProject(ctx context.Context, name string, remotes []string, by *pb.Stamp) (*pb.Project, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	res, err := q.InsertProjectIgnore(ctx, controllerdb.InsertProjectIgnoreParams{Name: name, CreatedAt: by.At})
	if err != nil {
		return nil, false, fmt.Errorf("add project %s: %w", name, err)
	}
	id, err := projectID(ctx, q, name)
	if err != nil {
		return nil, false, err
	}
	var changed bool
	if n, err := res.RowsAffected(); err != nil {
		return nil, false, err
	} else if n > 0 {
		changed = true
		if err := addProjectChange(ctx, q, id, projectCreated, "", by); err != nil {
			return nil, false, err
		}
	}
	for _, r := range remotes {
		res, err := q.InsertProjectRepoIgnore(ctx, controllerdb.InsertProjectRepoIgnoreParams{ProjectID: id, Remote: r})
		if err != nil {
			return nil, false, fmt.Errorf("add %s to project %s: %w", r, name, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, false, err
		} else if n > 0 {
			changed = true
			if err := addProjectChange(ctx, q, id, projectAdded, r, by); err != nil {
				return nil, false, err
			}
		}
	}
	p, err := project(ctx, q, name)
	if err != nil {
		return nil, false, err
	}
	return p, changed, tx.Commit()
}

// RemoveProjectRepos takes remotes (canonical) out of the project called
// name, all of them or, if it does not hold one, none.
func (s *Store) RemoveProjectRepos(ctx context.Context, name string, remotes []string, by *pb.Stamp) (*pb.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	id, err := projectID(ctx, q, name)
	if err != nil {
		return nil, err
	}
	for _, r := range remotes {
		res, err := q.DeleteProjectRepo(ctx, controllerdb.DeleteProjectRepoParams{ProjectID: id, Remote: r})
		if err != nil {
			return nil, fmt.Errorf("remove %s from project %s: %w", r, name, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, api.Errorf(api.CodeNotFound, "project %s does not hold %s", name, r).WithSuggestion("hotbed project ls")
		}
		if err := addProjectChange(ctx, q, id, projectRemoved, r, by); err != nil {
			return nil, err
		}
	}
	p, err := project(ctx, q, name)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// What changes to a project are called in its history.
const (
	projectCreated = "created"
	projectAdded   = "added"
	projectRemoved = "removed"
)

func addProjectChange(ctx context.Context, q *controllerdb.Queries, project int64, what, repo string, by *pb.Stamp) error {
	if err := q.InsertProjectChange(ctx, controllerdb.InsertProjectChangeParams{
		ProjectID: project, What: what, Repo: repo, At: by.At, Session: by.Session, Host: by.Host, Reason: by.Reason,
	}); err != nil {
		return fmt.Errorf("record project change: %w", err)
	}
	return nil
}

// ProjectHistory is every recorded change to the project called name,
// oldest first.
func (s *Store) ProjectHistory(ctx context.Context, name string) ([]*pb.ProjectChange, error) {
	id, err := projectID(ctx, s.q, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListProjectChanges(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ProjectChange, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.ProjectChange{
			Seq:  r.Seq,
			What: r.What,
			Repo: r.Repo,
			By:   &pb.Stamp{At: r.At, Session: r.Session, Host: r.Host, Reason: r.Reason},
		})
	}
	return out, nil
}

// Project is the project called name.
func (s *Store) Project(ctx context.Context, name string) (*pb.Project, error) {
	return project(ctx, s.q, name)
}

// Projects lists every project by name.
func (s *Store) Projects(ctx context.Context) ([]*pb.Project, error) {
	rows, err := s.q.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []*pb.Project{}, nil
	}
	repoRows, err := s.q.ListAllProjectRepos(ctx)
	if err != nil {
		return nil, err
	}
	return buildProjects(rows, repoRows)
}

// ProjectsHolding lists the projects that hold the repo remote.
func (s *Store) ProjectsHolding(ctx context.Context, remote string) ([]*pb.Project, error) {
	rows, err := s.q.ListProjectsHoldingRemote(ctx, remote)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []*pb.Project{}, nil
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	var repoRows []controllerdb.ProjectRepo
	for i := 0; i < len(ids); i += 500 {
		batch, err := s.q.ListProjectReposByProjectIDs(ctx, ids[i:min(i+500, len(ids))])
		if err != nil {
			return nil, err
		}
		repoRows = append(repoRows, batch...)
	}
	return buildProjects(rows, repoRows)
}

func buildProjects(rows []controllerdb.Project, repoRows []controllerdb.ProjectRepo) ([]*pb.Project, error) {
	reposByID := make(map[int64][]string, len(rows))
	for _, r := range repoRows {
		reposByID[r.ProjectID] = append(reposByID[r.ProjectID], r.Remote)
	}
	out := make([]*pb.Project, 0, len(rows))
	for _, row := range rows {
		repos := reposByID[row.ID]
		slices.Sort(repos)
		setup, err := unmarshal(row.Setup, &pb.Project{})
		if err != nil {
			return nil, fmt.Errorf("read project %s's setup: %w", row.Name, err)
		}
		out = append(out, &pb.Project{
			Name: row.Name, CreatedAt: row.CreatedAt, Repos: repos,
			Steps: setup.Steps, Hosts: setup.Hosts, ScratchEpoch: setup.ScratchEpoch, SetupRevision: setup.SetupRevision,
		})
	}
	return out, nil
}

func project(ctx context.Context, q *controllerdb.Queries, name string) (*pb.Project, error) {
	row, err := q.GetProjectByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, noProject(name)
	}
	if err != nil {
		return nil, err
	}
	repos, err := q.ListProjectRepos(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	slices.Sort(repos)
	setup, err := unmarshal(row.Setup, &pb.Project{})
	if err != nil {
		return nil, fmt.Errorf("read project %s's setup: %w", name, err)
	}
	return &pb.Project{
		Name: row.Name, CreatedAt: row.CreatedAt, Repos: repos,
		Steps: setup.Steps, Hosts: setup.Hosts, ScratchEpoch: setup.ScratchEpoch, SetupRevision: setup.SetupRevision,
	}, nil
}

// EditProjectSetup changes what builds the bases of the project called
// name (its steps, hosts and scratch epoch) as edit says, in one
// transaction, and returns the project as it leaves it.
func (s *Store) EditProjectSetup(ctx context.Context, name string, edit func(p *pb.Project) error) (*pb.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	p, err := project(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if err := edit(p); err != nil {
		return nil, err
	}
	p.SetupRevision++
	doc, err := marshal(&pb.Project{Steps: p.Steps, Hosts: p.Hosts, ScratchEpoch: p.ScratchEpoch, SetupRevision: p.SetupRevision})
	if err != nil {
		return nil, err
	}
	id, err := projectID(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if err := q.UpdateProjectSetup(ctx, controllerdb.UpdateProjectSetupParams{Setup: doc, ID: id}); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// LiveSessionsByProject counts each project's sessions that have not ended.
func (s *Store) LiveSessionsByProject(ctx context.Context) (map[string]int32, error) {
	rows, err := s.q.CountLiveSessionsByProject(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int32, len(rows))
	for _, r := range rows {
		out[r.Name] = int32(r.Count)
	}
	return out, nil
}

// SchedulesByProject counts each project's scheduled commands.
func (s *Store) SchedulesByProject(ctx context.Context) (map[string]int32, error) {
	rows, err := s.q.CountSchedulesByProject(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int32, len(rows))
	for _, r := range rows {
		out[r.Name] = int32(r.Count)
	}
	return out, nil
}

// projectID is the row of the project called name.
func projectID(ctx context.Context, q *controllerdb.Queries, name string) (int64, error) {
	id, err := q.GetProjectIDByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, noProject(name)
	}
	return id, err
}

func noProject(name string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no project %s", name).WithSuggestion("hotbed project ls; hotbed project add NAME --repo REMOTE makes one")
}
