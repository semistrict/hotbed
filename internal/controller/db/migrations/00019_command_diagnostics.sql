-- +goose Up
-- The diagnostics taken of commands as someone stopped or paused them by
-- hand: one protojson CommandDiagnostics per intervention.
CREATE TABLE command_diagnostics (
	command_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	node_id    TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	doc        TEXT NOT NULL,
	PRIMARY KEY (command_id, seq)
) STRICT;
CREATE INDEX command_diagnostics_created ON command_diagnostics (created_at);

-- +goose Down
DROP INDEX command_diagnostics_created;
DROP TABLE command_diagnostics;
