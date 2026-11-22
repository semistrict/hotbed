-- +goose Up
CREATE TABLE records (
	kind TEXT NOT NULL,   -- base, workspace, command
	id   TEXT NOT NULL,
	doc  TEXT NOT NULL,
	PRIMARY KEY (kind, id)
) STRICT;

-- Workers before the event stream queued what the controller had not
-- acknowledged here; one that still has items delivers them once.
CREATE TABLE outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,   -- finished, event, session
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
