-- +goose Up
-- Singleton row recording that this controller has migrated to another
-- controller. Once present, Heartbeat and PushEvents return migrated_to so
-- workers rewrite their controller config and reconnect to the target, and
-- mutating RPCs are refused.
CREATE TABLE controller_migration (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    target_controller  TEXT NOT NULL,
    controller_node_id TEXT NOT NULL DEFAULT '',
    default_token      TEXT NOT NULL DEFAULT '',
    worker_tokens_json TEXT NOT NULL DEFAULT '{}',
    migrated_at        INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE controller_migration;
