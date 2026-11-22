-- +goose Up
-- Always-on OpenTelemetry eBPF continuous profiles captured for finished commands.
CREATE TABLE command_profiles (
	id       TEXT PRIMARY KEY,
	node_id  TEXT NOT NULL,
	ended_at INTEGER NOT NULL,
	doc      BLOB NOT NULL
) STRICT;
CREATE INDEX command_profiles_ended ON command_profiles (ended_at);

-- +goose Down
DROP INDEX command_profiles_ended;
DROP TABLE command_profiles;
