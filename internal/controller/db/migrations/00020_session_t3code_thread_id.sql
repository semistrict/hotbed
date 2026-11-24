-- +goose Up
-- The T3 Code thread associated with a session (from T3CODE_THREAD_ID when
-- started inside T3 Code); empty otherwise.
ALTER TABLE sessions ADD COLUMN t3code_thread_id TEXT NOT NULL DEFAULT '';
CREATE INDEX sessions_t3code_thread_id ON sessions (t3code_thread_id, last_active_at) WHERE t3code_thread_id != '';

-- +goose Down
DROP INDEX sessions_t3code_thread_id;
ALTER TABLE sessions DROP COLUMN t3code_thread_id;
