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

// baselineVersion is the version of the squashed baseline migration. Databases
// recorded at a nonzero version below this predate the squash and must upgrade
// through the preceding release before running this binary.
const baselineVersion = 11

// Migrate brings the controller's SQLite database up to date automatically at
// startup using goose SQL and Go migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	provider, err := newProvider(db)
	if err != nil {
		return err
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("read controller schema version: %w", err)
	}
	if current > target {
		return fmt.Errorf("database schema version %d is newer than this hotbed (%d)", current, target)
	}
	if current > 0 && current < baselineVersion {
		return fmt.Errorf("database schema version %d predates squashed baseline %d; upgrade through the preceding release before running this hotbed", current, baselineVersion)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply controller migrations: %w", err)
	}
	return nil
}

// newProvider is the controller's migrations, SQL and Go, over db.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sqlFiles,
		goose.WithGoMigrations(
			goose.NewGoMigration(15, &goose.GoFunc{RunTx: typeSessionEvents}, nil),
			goose.NewGoMigration(18, &goose.GoFunc{RunTx: dropNodesAcked}, nil),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("init controller migrations: %w", err)
	}
	return provider, nil
}
