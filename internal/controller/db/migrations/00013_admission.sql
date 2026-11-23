-- +goose Up
ALTER TABLE samples ADD COLUMN cpu_psi REAL NOT NULL DEFAULT 0; -- CPU pressure, some avg10
ALTER TABLE samples ADD COLUMN holding INTEGER NOT NULL DEFAULT 0; -- the node held new work

-- Where the controller put each workspace, and why (a PlacementDecision).
CREATE TABLE placements (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	at         INTEGER NOT NULL,
	session_id TEXT NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX placements_at ON placements (at);

-- +goose Down
DROP TABLE placements;
ALTER TABLE samples DROP COLUMN holding;
ALTER TABLE samples DROP COLUMN cpu_psi;
