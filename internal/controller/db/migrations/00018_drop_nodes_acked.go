package migrations

import (
	"context"
	"database/sql"
	"fmt"
)

// dropNodesAcked (migration 18) drops the retired nodes.acked column when
// present (pre-squash live controller databases at v11..v17) and is a no-op
// on databases initialized from the released squashed 00011_baseline.sql
// (which omitted acked), so both layouts converge to the exact same schema.
func dropNodesAcked(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(nodes)`)
	if err != nil {
		return fmt.Errorf("inspect nodes columns: %w", err)
	}
	hasAcked := false
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan nodes column: %w", err)
		}
		if name == "acked" {
			hasAcked = true
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil && closeErr != nil {
		return fmt.Errorf("close nodes table_info: %w", closeErr)
	}
	if err != nil {
		return fmt.Errorf("iterate nodes columns: %w", err)
	}
	if !hasAcked {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE nodes DROP COLUMN acked`); err != nil {
		return fmt.Errorf("drop nodes.acked: %w", err)
	}
	return nil
}
