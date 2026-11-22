-- +goose Up
-- Reviews of an exact version by reviewer agents, their findings, and
-- every change and response to them.
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
