-- +goose Up
-- Every session and schedule belongs to a project (migration 6 backfilled any
-- legacy rows before this schema change enforces NOT NULL + foreign keys).
CREATE TABLE sessions_new (
	id             TEXT PRIMARY KEY,
	agent          TEXT NOT NULL,
	project_id     INTEGER NOT NULL REFERENCES projects (id),
	started_at     INTEGER NOT NULL,
	last_active_at INTEGER NOT NULL,
	ended_at       INTEGER,
	end_reason     TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO sessions_new (id, agent, project_id, started_at, last_active_at, ended_at, end_reason)
	SELECT id, agent, project_id, started_at, last_active_at, ended_at, end_reason FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX sessions_project ON sessions (project_id, ended_at);

CREATE TABLE schedules_new (
	name       TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects (id),
	doc        TEXT NOT NULL, -- the ScheduledCommand, without project, next_at and runs
	next_at    INTEGER        -- unix ms; NULL while paused
) STRICT;
INSERT INTO schedules_new (name, project_id, doc, next_at)
	SELECT s.name, p.id, json_remove(s.doc, '$.project'), s.next_at
	FROM schedules s
	JOIN projects p ON p.name = json_extract(s.doc, '$.project');
DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;
CREATE INDEX schedules_project ON schedules (project_id);

-- History entries remember their project directly (nullable for sessionless
-- freshen commands whose repo may belong to multiple projects).
ALTER TABLE history ADD COLUMN project_id INTEGER REFERENCES projects (id);
UPDATE history SET project_id = (
	SELECT s.project_id FROM sessions s WHERE s.id = json_extract(history.doc, '$.sessionId')
) WHERE json_extract(history.doc, '$.sessionId') IS NOT NULL AND json_extract(history.doc, '$.sessionId') != '';
CREATE INDEX history_project ON history (project_id, ended_at);

-- +goose Down
DROP INDEX history_project;
ALTER TABLE history DROP COLUMN project_id;
DROP INDEX schedules_project;
CREATE TABLE schedules_old (
	name    TEXT PRIMARY KEY,
	doc     TEXT NOT NULL,
	next_at INTEGER
) STRICT;
INSERT INTO schedules_old (name, doc, next_at)
	SELECT s.name, json_set(s.doc, '$.project', p.name), s.next_at
	FROM schedules s
	JOIN projects p ON p.id = s.project_id;
DROP TABLE schedules;
ALTER TABLE schedules_old RENAME TO schedules;
DROP INDEX sessions_project;
CREATE TABLE sessions_old (
	id             TEXT PRIMARY KEY,
	agent          TEXT NOT NULL,
	started_at     INTEGER NOT NULL,
	last_active_at INTEGER NOT NULL,
	ended_at       INTEGER,
	end_reason     TEXT NOT NULL DEFAULT '',
	project_id     INTEGER REFERENCES projects (id)
) STRICT;
INSERT INTO sessions_old (id, agent, started_at, last_active_at, ended_at, end_reason, project_id)
	SELECT id, agent, started_at, last_active_at, ended_at, end_reason, project_id FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_old RENAME TO sessions;
