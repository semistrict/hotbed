package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// querier is what reads the database: the store's, or a transaction's.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AddProject makes the project called name, unless it is there already,
// and adds remotes (canonical) to the repos it holds, recording each change
// as by's.
func (s *Store) AddProject(ctx context.Context, name string, remotes []string, by *pb.Stamp) (*pb.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO projects (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, by.At)
	if err != nil {
		return nil, fmt.Errorf("add project %s: %w", name, err)
	}
	id, err := projectID(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n > 0 {
		if err := addProjectChange(ctx, tx, id, projectCreated, "", by); err != nil {
			return nil, err
		}
	}
	for _, r := range remotes {
		res, err := tx.ExecContext(ctx, `INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, r)
		if err != nil {
			return nil, fmt.Errorf("add %s to project %s: %w", r, name, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n > 0 {
			if err := addProjectChange(ctx, tx, id, projectAdded, r, by); err != nil {
				return nil, err
			}
		}
	}
	p, err := project(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// RemoveProjectRepos takes remotes (canonical) out of the project called
// name, all of them or, if it does not hold one, none.
func (s *Store) RemoveProjectRepos(ctx context.Context, name string, remotes []string, by *pb.Stamp) (*pb.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := projectID(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	for _, r := range remotes {
		res, err := tx.ExecContext(ctx, `DELETE FROM project_repos WHERE project_id = ? AND remote = ?`, id, r)
		if err != nil {
			return nil, fmt.Errorf("remove %s from project %s: %w", r, name, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, api.Errorf(api.CodeNotFound, "project %s does not hold %s", name, r).WithSuggestion("hotbed project ls")
		}
		if err := addProjectChange(ctx, tx, id, projectRemoved, r, by); err != nil {
			return nil, err
		}
	}
	p, err := project(ctx, tx, name)
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

func addProjectChange(ctx context.Context, tx *sql.Tx, project int64, what, repo string, by *pb.Stamp) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		project, what, repo, by.At, by.Session, by.Host, by.Reason)
	if err != nil {
		return fmt.Errorf("record project change: %w", err)
	}
	return nil
}

// ProjectHistory is every recorded change to the project called name,
// oldest first.
func (s *Store) ProjectHistory(ctx context.Context, name string) ([]*pb.ProjectChange, error) {
	id, err := projectID(ctx, s.db, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, what, repo, at, session, host, reason FROM project_changes WHERE project_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.ProjectChange
	for rows.Next() {
		c := &pb.ProjectChange{By: &pb.Stamp{}}
		if err := rows.Scan(&c.Seq, &c.What, &c.Repo, &c.By.At, &c.By.Session, &c.By.Host, &c.By.Reason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Project is the project called name.
func (s *Store) Project(ctx context.Context, name string) (*pb.Project, error) {
	return project(ctx, s.db, name)
}

// Projects lists every project by name.
func (s *Store) Projects(ctx context.Context) ([]*pb.Project, error) {
	return projects(ctx, s.db, `SELECT name FROM projects ORDER BY name`)
}

// ProjectsHolding lists the projects that hold the repo remote.
func (s *Store) ProjectsHolding(ctx context.Context, remote string) ([]*pb.Project, error) {
	return projects(ctx, s.db, `SELECT p.name FROM projects p JOIN project_repos r ON r.project_id = p.id WHERE r.remote = ? ORDER BY p.name`, remote)
}

func projects(ctx context.Context, q querier, query string, args ...any) ([]*pb.Project, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*pb.Project, 0, len(names))
	for _, n := range names {
		p, err := project(ctx, q, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func project(ctx context.Context, q querier, name string) (*pb.Project, error) {
	p := &pb.Project{Name: name}
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id, created_at FROM projects WHERE name = ?`, name).Scan(&id, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, noProject(name)
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT remote FROM project_repos WHERE project_id = ? ORDER BY remote`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		p.Repos = append(p.Repos, r)
	}
	slices.Sort(p.Repos)
	return p, rows.Err()
}

// LiveSessionsByProject counts each project's sessions that have not ended.
func (s *Store) LiveSessionsByProject(ctx context.Context) (map[string]int32, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.name, COUNT(*) FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.ended_at IS NULL GROUP BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int32{}
	for rows.Next() {
		var name string
		var n int32
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// projectID is the row of the project called name.
func projectID(ctx context.Context, q querier, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, noProject(name)
	}
	return id, err
}

// projectlessSession is a session whose project_id is NULL, with the repos
// of the workspaces it has held, oldest first.
type projectlessSession struct {
	id    string
	repos []string
}

// SessionsWithoutProject lists sessions that have no project yet, together
// with the repo IDs of any workspaces they created.
func (s *Store) SessionsWithoutProject(ctx context.Context) ([]projectlessSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, COALESCE(w.repo_id, '')
		FROM sessions s
		LEFT JOIN workspaces w ON w.session_id = s.id
		WHERE s.project_id IS NULL
		ORDER BY s.started_at, w.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectlessSession
	idx := map[string]int{}
	for rows.Next() {
		var id, repo string
		if err := rows.Scan(&id, &repo); err != nil {
			return nil, err
		}
		i, ok := idx[id]
		if !ok {
			i = len(out)
			idx[id] = i
			out = append(out, projectlessSession{id: id})
		}
		if repo != "" && !slices.Contains(out[i].repos, repo) {
			out[i].repos = append(out[i].repos, repo)
		}
	}
	return out, rows.Err()
}

// SetSessionProject attaches a session that has no project yet to project.
func (s *Store) SetSessionProject(ctx context.Context, sessionID, project string) error {
	pid, err := projectID(ctx, s.db, project)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET project_id = ? WHERE id = ? AND project_id IS NULL`, pid, sessionID)
	return err
}

func noProject(name string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no project %s", name).WithSuggestion("hotbed project ls; hotbed project add NAME --repo REMOTE makes one")
}
