-- +goose Up
-- Baseline schema as of migration 2 (squashed from historical migrations 1..2).
-- Live worker databases already at goose_db_version >= 2 skip this migration cleanly.

CREATE TABLE records (
	kind TEXT NOT NULL, -- base, workspace, command
	id   TEXT NOT NULL,
	doc  TEXT NOT NULL,
	PRIMARY KEY (kind, id)
) STRICT;

-- Present in historical v1..v2 worker databases; dropped by migration 3.
CREATE TABLE outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL, -- finished, event, session
	doc  TEXT NOT NULL
) STRICT;

-- AUTOINCREMENT: a sequence number is never used twice, even once pruned.
CREATE TABLE events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	at  INTEGER NOT NULL, -- unix ms
	doc TEXT NOT NULL     -- an Event, without its seq
) STRICT;

CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE meta;
DROP TABLE events;
DROP TABLE outbox;
DROP TABLE records;
