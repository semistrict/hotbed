package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

//go:embed *.sql
var sqlFiles embed.FS

// legacyMaxVersion is the highest PRAGMA user_version written by the old
// hand-rolled slice before goose took over schema management.
const legacyMaxVersion = 6

var (
	validProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	notInProjectName = regexp.MustCompile(`[^a-z0-9]+`)
)

// Migrate brings the controller's SQLite database up to date automatically at
// startup. Pre-goose databases tracked via PRAGMA user_version (1..6) are
// adopted into goose_db_version before applying newer SQL and Go migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	if err := adoptLegacy(ctx, db); err != nil {
		return err
	}
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sqlFiles,
		goose.WithGoMigrations(
			goose.NewGoMigration(7, &goose.GoFunc{RunTx: backfillProjects}, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("init controller migrations: %w", err)
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("read controller schema version: %w", err)
	}
	if current > target {
		return fmt.Errorf("database schema version %d is newer than this hotbed (%d)", current, target)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply controller migrations: %w", err)
	}
	return nil
}

// adoptLegacy copies an existing PRAGMA user_version (1..5) into
// goose_db_version so databases created before goose continue smoothly from
// migration 6 without re-running migrations 1..5.
func adoptLegacy(ctx context.Context, db *sql.DB) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, goose.DefaultTablename).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose > 0 {
		return nil
	}
	var userVersion int64
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return err
	}
	if userVersion == 0 {
		return nil
	}
	if userVersion > legacyMaxVersion {
		return fmt.Errorf("database schema version %d is newer than this hotbed (%d)", userVersion, legacyMaxVersion)
	}
	store, err := database.NewStore(goose.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.CreateVersionTable(ctx, tx); err != nil {
		return err
	}
	for v := int64(0); v <= userVersion; v++ {
		if err := store.Insert(ctx, tx, database.InsertRequest{Version: v}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillProjects (migration 6) ensures every registered repo, every session
// with NULL project_id, and every schedule belongs to a project before
// migration 7 enforces NOT NULL foreign keys in SQL.
func backfillProjects(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UnixMilli()
	unmarshalOpts := protojson.UnmarshalOptions{DiscardUnknown: true}

	// 1. Ensure every registered repo belongs to at least one project.
	repoRemote := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, doc FROM repos ORDER BY id`)
	if err != nil {
		return err
	}
	type repoRec struct {
		id, url string
	}
	var repos []repoRec
	for rows.Next() {
		var id, doc string
		if err := rows.Scan(&id, &doc); err != nil {
			rows.Close()
			return err
		}
		var r pb.Repo
		if err := unmarshalOpts.Unmarshal([]byte(doc), &r); err != nil {
			rows.Close()
			return fmt.Errorf("decode repo %s: %w", id, err)
		}
		repos = append(repos, repoRec{id: id, url: r.Url})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range repos {
		remote := gitops.CanonicalRemote(r.url)
		if remote == "" {
			continue
		}
		repoRemote[r.id] = remote
		if _, err := ensureRepoProjectTx(ctx, tx, r.id, remote, now); err != nil {
			return err
		}
	}

	// 2. Backfill any sessions whose project_id is NULL.
	sessRows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE project_id IS NULL ORDER BY started_at`)
	if err != nil {
		return err
	}
	var orphanSessions []string
	for sessRows.Next() {
		var id string
		if err := sessRows.Scan(&id); err != nil {
			sessRows.Close()
			return err
		}
		orphanSessions = append(orphanSessions, id)
	}
	sessRows.Close()
	if err := sessRows.Err(); err != nil {
		return err
	}

	// Preload schedules (name -> raw doc) for schedule-run inference and backfill.
	schedRows, err := tx.QueryContext(ctx, `SELECT name, doc FROM schedules ORDER BY name`)
	if err != nil {
		return err
	}
	type schedRec struct {
		name string
		sc   *pb.ScheduledCommand
	}
	var scheds []schedRec
	schedByName := map[string]*pb.ScheduledCommand{}
	for schedRows.Next() {
		var name, doc string
		if err := schedRows.Scan(&name, &doc); err != nil {
			schedRows.Close()
			return err
		}
		sc := &pb.ScheduledCommand{}
		if err := unmarshalOpts.Unmarshal([]byte(doc), sc); err != nil {
			schedRows.Close()
			return fmt.Errorf("decode schedule %s: %w", name, err)
		}
		scheds = append(scheds, schedRec{name: name, sc: sc})
		schedByName[name] = sc
	}
	schedRows.Close()
	if err := schedRows.Err(); err != nil {
		return err
	}

	// Ensure every schedule has a project in its doc and that the project exists.
	for _, item := range scheds {
		sc := item.sc
		var pid int64
		var pname string
		if sc.Project != "" {
			var err error
			pid, err = ensureProjectByNameTx(ctx, tx, sc.Project, now)
			if err != nil {
				return err
			}
			pname = sc.Project
		} else {
			if remote := repoRemote[sc.Repo]; remote != "" {
				var err error
				pid, pname, err = firstProjectHoldingTx(ctx, tx, remote)
				if err != nil {
					return err
				}
			}
			if pid == 0 {
				var err error
				pid, err = ensureProjectByNameTx(ctx, tx, "default", now)
				if err != nil {
					return err
				}
				pname = "default"
			}
		}
		if sc.Project != pname {
			sc.Project = pname
			b, err := protojson.Marshal(sc)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE schedules SET doc = ? WHERE name = ?`, string(b), item.name); err != nil {
				return err
			}
		}
		_ = pid
	}

	for _, sessID := range orphanSessions {
		pid, err := inferSessionProjectTx(ctx, tx, sessID, repoRemote, schedByName, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET project_id = ? WHERE id = ?`, pid, sessID); err != nil {
			return err
		}
	}
	return nil
}

func inferSessionProjectTx(ctx context.Context, tx *sql.Tx, sessID string, repoRemote map[string]string, schedByName map[string]*pb.ScheduledCommand, now int64) (int64, error) {
	// 1. From workspaces' repos.
	wsRows, err := tx.QueryContext(ctx, `SELECT repo_id FROM workspaces WHERE session_id = ? ORDER BY created_at`, sessID)
	if err != nil {
		return 0, err
	}
	var repos []string
	for wsRows.Next() {
		var r string
		if err := wsRows.Scan(&r); err != nil {
			wsRows.Close()
			return 0, err
		}
		repos = append(repos, r)
	}
	wsRows.Close()
	if err := wsRows.Err(); err != nil {
		return 0, err
	}
	for _, repoID := range repos {
		if remote := repoRemote[repoID]; remote != "" {
			pid, _, err := firstProjectHoldingTx(ctx, tx, remote)
			if err != nil {
				return 0, err
			}
			if pid > 0 {
				return pid, nil
			}
		}
	}

	// 2. From schedule_runs.
	var schedName string
	if err := tx.QueryRowContext(ctx, `SELECT schedule FROM schedule_runs WHERE session_id = ? LIMIT 1`, sessID).Scan(&schedName); err == nil && schedName != "" {
		if sc := schedByName[schedName]; sc != nil && sc.Project != "" {
			return ensureProjectByNameTx(ctx, tx, sc.Project, now)
		}
	}

	// 3. From questions asked by this session.
	var pid int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM questions WHERE asked_by = ? LIMIT 1`, sessID).Scan(&pid); err == nil && pid > 0 {
		return pid, nil
	}

	// 4. From reviews requested by this session.
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM reviews WHERE requested_by = ? LIMIT 1`, sessID).Scan(&pid); err == nil && pid > 0 {
		return pid, nil
	}

	// 5. Fallback for legacy empty sessions with no workspaces or activity.
	return ensureProjectByNameTx(ctx, tx, "default", now)
}

func firstProjectHoldingTx(ctx context.Context, tx *sql.Tx, remote string) (int64, string, error) {
	var id int64
	var name string
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, p.name
		FROM projects p
		JOIN project_repos r ON r.project_id = p.id
		WHERE r.remote = ?
		ORDER BY p.name
		LIMIT 1`, remote).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, name, err
}

func ensureProjectByNameTx(ctx context.Context, tx *sql.Tx, name string, now int64) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, now); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, name).Scan(&id)
	return id, err
}

func ensureRepoProjectTx(ctx context.Context, tx *sql.Tx, repoID, remote string, now int64) (int64, error) {
	if id, _, err := firstProjectHoldingTx(ctx, tx, remote); err != nil {
		return 0, err
	} else if id > 0 {
		return id, nil
	}
	var candidates []string
	if own := projectNameFor(remote); own != "" {
		candidates = append(candidates, own)
	}
	if repoID != "" && (len(candidates) == 0 || candidates[0] != repoID) && validProjectName.MatchString(repoID) {
		candidates = append(candidates, repoID)
	}
	base := "repo"
	if len(candidates) > 0 {
		base = candidates[0]
	}
	for n := 2; n <= 99; n++ {
		suffix := fmt.Sprintf("-%d", n)
		prefix := base
		if len(prefix)+len(suffix) > 40 {
			prefix = strings.TrimRight(prefix[:40-len(suffix)], "-")
		}
		candidates = append(candidates, prefix+suffix)
	}
	for _, name := range candidates {
		var existingID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, name).Scan(&existingID)
		if err == nil {
			var repoCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_repos WHERE project_id = ?`, existingID).Scan(&repoCount); err != nil {
				return 0, err
			}
			if repoCount > 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING`, existingID, remote); err != nil {
				return 0, err
			}
			return existingID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		id, err := ensureProjectByNameTx(ctx, tx, name, now)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, remote); err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, fmt.Errorf("no free project name for repo %s (%s)", repoID, remote)
}

func projectNameFor(remote string) string {
	name := strings.ToLower(remote[strings.LastIndex(remote, "/")+1:])
	name = strings.Trim(notInProjectName.ReplaceAllString(name, "-"), "-")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	return name
}
