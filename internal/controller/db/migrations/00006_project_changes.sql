-- +goose Up
-- Every change to a project from now on: created, and repos added and
-- removed, with who and when.
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

-- +goose Down
DROP INDEX IF EXISTS project_changes_project;
DROP TABLE IF EXISTS project_changes;
