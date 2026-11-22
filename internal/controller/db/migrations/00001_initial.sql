-- +goose Up
CREATE TABLE nodes (
	id        TEXT PRIMARY KEY, -- tailnet stable node ID
	hostname  TEXT NOT NULL,
	login     TEXT NOT NULL,
	addr      TEXT NOT NULL DEFAULT '', -- tailnet address, to call it
	joined_at INTEGER NOT NULL, -- unix ms
	last_seen INTEGER,
	report    TEXT,             -- the latest ReportRequest, without its outbox
	acked     INTEGER NOT NULL DEFAULT 0 -- highest outbox seq stored
) STRICT;

CREATE TABLE repos (
	id  TEXT PRIMARY KEY,
	doc TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
	id             TEXT PRIMARY KEY,
	agent          TEXT NOT NULL,
	started_at     INTEGER NOT NULL,
	last_active_at INTEGER NOT NULL,
	ended_at       INTEGER,
	end_reason     TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE workspaces (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	node_id     TEXT NOT NULL,
	repo_id     TEXT NOT NULL,
	agent       TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	fork_millis INTEGER,
	released_at INTEGER
) STRICT;
CREATE INDEX workspaces_session ON workspaces (session_id);
CREATE INDEX workspaces_created ON workspaces (created_at);

CREATE TABLE commands (
	id           TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	node_id      TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE session_events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX session_events_session ON session_events (session_id, seq);

CREATE TABLE history (
	id       TEXT PRIMARY KEY,
	node_id  TEXT NOT NULL,
	repo_id  TEXT NOT NULL,
	ended_at INTEGER NOT NULL,
	doc      TEXT NOT NULL
) STRICT;
CREATE INDEX history_ended ON history (ended_at);

CREATE TABLE samples (
	node_id TEXT NOT NULL,
	t       INTEGER NOT NULL,
	cpu     REAL NOT NULL,
	memory  REAL NOT NULL,     -- share of RAM in use
	psi     REAL NOT NULL,     -- memory pressure, some avg10
	running INTEGER NOT NULL,
	queued  INTEGER NOT NULL,
	parked  INTEGER NOT NULL,
	oom     INTEGER NOT NULL,  -- kernel OOM kills since boot
	PRIMARY KEY (node_id, t)
) STRICT;
CREATE INDEX samples_t ON samples (t);

CREATE TABLE events (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	at       INTEGER NOT NULL,
	kind     TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	text     TEXT NOT NULL,
	ref_type TEXT NOT NULL,
	ref_id   TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE events;
DROP TABLE samples;
DROP TABLE history;
DROP TABLE session_events;
DROP TABLE commands;
DROP TABLE workspaces;
DROP TABLE sessions;
DROP TABLE repos;
DROP TABLE nodes;
