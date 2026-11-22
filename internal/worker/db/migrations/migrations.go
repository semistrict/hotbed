package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

//go:embed *.sql
var sqlFiles embed.FS

// Migrate brings the worker's SQLite database up to date automatically at
// startup. Pre-goose databases that already have the records table are
// adopted at version 1 before running any newer migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	if err := adoptLegacy(ctx, db); err != nil {
		return err
	}
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
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply worker migrations: %w", err)
	}
	return nil
}

// adoptLegacy records version 1 in goose_db_version when opening a worker
// database created before goose managed its schema.
func adoptLegacy(ctx context.Context, db *sql.DB) error {
	var hasGoose int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, goose.DefaultTablename).Scan(&hasGoose); err != nil {
		return err
	}
	if hasGoose > 0 {
		return nil
	}
	var hasRecords int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'records'`).Scan(&hasRecords); err != nil {
		return err
	}
	if hasRecords == 0 {
		return nil
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
	for _, v := range []int64{0, 1} {
		if err := store.Insert(ctx, tx, database.InsertRequest{Version: v}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
