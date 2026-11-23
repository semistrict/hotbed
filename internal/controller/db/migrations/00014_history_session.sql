-- +goose Up
-- The session a finished command ran in, so ps can find a session's history
-- by index; empty for a base's freshen.
ALTER TABLE history ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
UPDATE history SET session_id = COALESCE(json_extract(doc, '$.sessionId'), '');
CREATE INDEX history_session ON history (session_id, ended_at);

-- +goose Down
DROP INDEX history_session;
ALTER TABLE history DROP COLUMN session_id;
