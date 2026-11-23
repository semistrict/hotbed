-- +goose Up
CREATE INDEX commands_session ON commands (session_id);
CREATE INDEX commands_created ON commands (created_at);

-- +goose Down
DROP INDEX commands_created;
DROP INDEX commands_session;
