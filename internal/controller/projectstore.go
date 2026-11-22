package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

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
// and adds remotes (canonical) to the repos it holds.
func (s *Store) AddProject(ctx context.Context, name string, remotes []string, now time.Time) (*pb.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("add project %s: %w", name, err)
	}
	id, err := projectID(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	for _, r := range remotes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, r); err != nil {
			return nil, fmt.Errorf("add %s to project %s: %w", r, name, err)
		}
	}
	p, err := project(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
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

// projectID is the row of the project called name.
func projectID(ctx context.Context, q querier, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, noProject(name)
	}
	return id, err
}

func noProject(name string) *api.Error {
	return api.Errorf(api.CodeNotFound, "no project %s", name).WithSuggestion("hotbed project ls; hotbed project add NAME --repo REMOTE makes one")
}
