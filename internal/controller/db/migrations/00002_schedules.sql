-- +goose Up
CREATE TABLE schedules (
	name    TEXT PRIMARY KEY,
	doc     TEXT NOT NULL,  -- the ScheduledCommand, without next_at and runs
	next_at INTEGER         -- unix ms; NULL while paused
) STRICT;

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

-- +goose Down
DROP TABLE schedule_runs;
DROP TABLE schedules;
