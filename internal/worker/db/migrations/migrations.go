package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var sqlFiles embed.FS

// baselineVersion is the version of the squashed baseline migration. Worker
// databases recorded at a nonzero version below this predate the squash and
// must upgrade through the preceding release before running this binary.
const baselineVersion = 2

// Migrate brings the worker's SQLite database up to date automatically at
// startup using goose SQL migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sqlFiles)
	if err != nil {
		return fmt.Errorf("init worker migrations: %w", err)
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("read worker schema version: %w", err)
	}
	if current > target {
		return fmt.Errorf("worker database schema version %d is newer than this hotbed (%d)", current, target)
	}
	if current > 0 && current < baselineVersion {
		return fmt.Errorf("worker database schema version %d predates squashed baseline %d; upgrade through the preceding release before running this hotbed", current, baselineVersion)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply worker migrations: %w", err)
	}
	return nil
}
