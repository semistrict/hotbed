-- +goose Up
-- Projects, which sessions work on and questions are about; questions
-- agents ask the user, and every change made to them.
CREATE TABLE projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE project_repos (
	project_id INTEGER NOT NULL REFERENCES projects (id),
	remote     TEXT NOT NULL, -- canonical git remote: github.com/semistrict/hotbed
	PRIMARY KEY (project_id, remote)
) STRICT;
CREATE INDEX project_repos_remote ON project_repos (remote);

ALTER TABLE sessions ADD COLUMN project_id INTEGER REFERENCES projects (id);

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

-- +goose Down
DROP TABLE question_changes;
DROP TABLE questions;
ALTER TABLE sessions DROP COLUMN project_id;
DROP TABLE project_repos;
DROP TABLE projects;
