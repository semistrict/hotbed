-- name: GetControllerMigration :one
SELECT target_controller, controller_node_id, default_token, worker_tokens_json, migrated_at
FROM controller_migration
WHERE id = 1;

-- name: SetControllerMigration :exec
INSERT INTO controller_migration (id, target_controller, controller_node_id, default_token, worker_tokens_json, migrated_at)
VALUES (1, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    target_controller = excluded.target_controller,
    controller_node_id = excluded.controller_node_id,
    default_token = excluded.default_token,
    worker_tokens_json = excluded.worker_tokens_json,
    migrated_at = excluded.migrated_at;
