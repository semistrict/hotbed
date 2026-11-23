-- +goose Up
-- Baseline schema as of migration 11 (squashed from historical migrations 1..11).
-- Live databases already at goose_db_version >= 11 skip this migration cleanly.

CREATE TABLE nodes (
	id         TEXT PRIMARY KEY, -- tailnet stable node ID
	hostname   TEXT NOT NULL,
	login      TEXT NOT NULL,
	addr       TEXT NOT NULL DEFAULT '', -- tailnet address, to call it
	joined_at  INTEGER NOT NULL,         -- unix ms
	last_seen  INTEGER,
	epoch      TEXT NOT NULL DEFAULT '',
	applied    INTEGER NOT NULL DEFAULT 0,
	applied_at INTEGER NOT NULL DEFAULT 0,
	heartbeat  TEXT
) STRICT;

CREATE TABLE node_state (
	node_id TEXT NOT NULL,
	kind    TEXT NOT NULL, -- base, workspace, command
	id      TEXT NOT NULL,
	doc     TEXT NOT NULL,
	PRIMARY KEY (node_id, kind, id)
) STRICT;

CREATE TABLE repos (
	id  TEXT PRIMARY KEY,
	doc TEXT NOT NULL
) STRICT;

CREATE TABLE projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	setup      TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE TABLE project_repos (
	project_id INTEGER NOT NULL REFERENCES projects (id),
	remote     TEXT NOT NULL, -- canonical git remote: github.com/semistrict/hotbed
	PRIMARY KEY (project_id, remote)
) STRICT;
CREATE INDEX project_repos_remote ON project_repos (remote);

CREATE TABLE project_changes (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	what       TEXT NOT NULL, -- created, added, removed
	repo       TEXT NOT NULL, -- the repo added or removed; empty for created
	at         INTEGER NOT NULL,
	session    TEXT NOT NULL, -- empty for a person, or the controller
	host       TEXT NOT NULL,
	reason     TEXT NOT NULL
) STRICT;
CREATE INDEX project_changes_project ON project_changes (project_id, seq);

CREATE TABLE sessions (
	id             TEXT PRIMARY KEY,
	agent          TEXT NOT NULL,
	project_id     INTEGER NOT NULL REFERENCES projects (id),
	started_at     INTEGER NOT NULL,
	last_active_at INTEGER NOT NULL,
	ended_at       INTEGER,
	end_reason     TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX sessions_project ON sessions (project_id, ended_at);

CREATE TABLE workspaces (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	node_id     TEXT NOT NULL,
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
	id         TEXT PRIMARY KEY,
	node_id    TEXT NOT NULL,
	repo_id    TEXT NOT NULL,
	ended_at   INTEGER NOT NULL,
	doc        TEXT NOT NULL,
	project_id INTEGER REFERENCES projects (id)
) STRICT;
CREATE INDEX history_ended ON history (ended_at);
CREATE INDEX history_project ON history (project_id, ended_at);

CREATE TABLE command_profiles (
	id       TEXT PRIMARY KEY,
	node_id  TEXT NOT NULL,
	ended_at INTEGER NOT NULL,
	doc      BLOB NOT NULL
) STRICT;
CREATE INDEX command_profiles_ended ON command_profiles (ended_at);

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

CREATE TABLE schedules (
	name       TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	doc        TEXT NOT NULL, -- the ScheduledCommand, without project, next_at and runs
	next_at    INTEGER        -- unix ms; NULL while paused
) STRICT;
CREATE INDEX schedules_project ON schedules (project_id);

CREATE TABLE schedule_runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	schedule   TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	command_id TEXT NOT NULL DEFAULT '',
	ended_at   INTEGER,     -- NULL while it runs
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX schedule_runs_schedule ON schedule_runs (schedule, id);
CREATE INDEX schedule_runs_session ON schedule_runs (session_id);
CREATE INDEX schedule_runs_live ON schedule_runs (ended_at) WHERE ended_at IS NULL;

CREATE TABLE questions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	repo       TEXT NOT NULL,    -- the project's repo it is about; empty for the whole project
	state      TEXT NOT NULL,    -- QuestionState's name
	rank       INTEGER NOT NULL, -- the user's order, lowest first
	asked_by   TEXT NOT NULL,    -- the asking session
	asked_at   INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	doc        TEXT NOT NULL     -- the Question, without its project, rank and history
) STRICT;
CREATE INDEX questions_project ON questions (project_id, state);

CREATE TABLE question_changes (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	question_id INTEGER NOT NULL REFERENCES questions (id),
	what        TEXT NOT NULL,
	at          INTEGER NOT NULL,
	session     TEXT NOT NULL, -- empty for the user
	host        TEXT NOT NULL,
	reason      TEXT NOT NULL,
	doc         TEXT NOT NULL  -- the Question as the change left it
) STRICT;
CREATE INDEX question_changes_question ON question_changes (question_id, seq);

CREATE TABLE reviews (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id   INTEGER NOT NULL REFERENCES projects (id),
	state        TEXT NOT NULL, -- ReviewState's name
	requested_by TEXT NOT NULL, -- the requesting session
	requested_at INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	doc          TEXT NOT NULL  -- the Review, without its project, findings, events and counts
) STRICT;
CREATE INDEX reviews_project ON reviews (project_id, state);

CREATE TABLE review_findings (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	review_id INTEGER NOT NULL REFERENCES reviews (id),
	doc       TEXT NOT NULL -- the Finding, without its responses
) STRICT;
CREATE INDEX review_findings_review ON review_findings (review_id, id);

CREATE TABLE review_events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	review_id  INTEGER NOT NULL REFERENCES reviews (id),
	finding_id INTEGER NOT NULL, -- 0 for the review as a whole
	what       TEXT NOT NULL,
	at         INTEGER NOT NULL,
	session    TEXT NOT NULL,    -- empty for the user
	host       TEXT NOT NULL,
	quick      TEXT NOT NULL,    -- QuickResponse's name, for a response
	text       TEXT NOT NULL,
	doc        TEXT NOT NULL     -- the review or finding as the change left it; empty for a response
) STRICT;
CREATE INDEX review_events_review ON review_events (review_id, seq);

-- +goose Down
DROP TABLE review_events;
DROP TABLE review_findings;
DROP TABLE reviews;
DROP TABLE question_changes;
DROP TABLE questions;
DROP TABLE schedule_runs;
DROP TABLE schedules;
DROP TABLE events;
DROP TABLE samples;
DROP TABLE command_profiles;
DROP TABLE history;
DROP TABLE session_events;
DROP TABLE commands;
DROP TABLE workspaces;
DROP TABLE sessions;
DROP TABLE project_changes;
DROP TABLE project_repos;
DROP TABLE projects;
DROP TABLE repos;
DROP TABLE node_state;
DROP TABLE nodes;
