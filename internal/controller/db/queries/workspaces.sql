-- name: InsertWorkspace :exec
INSERT INTO workspaces (id, session_id, node_id, agent, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: InsertWorkspaceIfSessionLive :execrows
INSERT INTO workspaces (id, session_id, node_id, agent, created_at)
SELECT CAST(sqlc.arg(id) AS TEXT),
       CAST(sqlc.arg(session_id) AS TEXT),
       CAST(sqlc.arg(node_id) AS TEXT),
       CAST(sqlc.arg(agent) AS TEXT),
       CAST(sqlc.arg(created_at) AS INTEGER)
WHERE EXISTS (SELECT 1 FROM sessions WHERE id = sqlc.arg(session_id) AND ended_at IS NULL);

-- name: InsertWorkspaceIgnore :exec
INSERT OR IGNORE INTO workspaces (id, session_id, node_id, agent, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = ?;

-- name: ReleaseWorkspace :exec
UPDATE workspaces SET released_at = ? WHERE id = ?;

-- name: ReleaseWorkspaceIfLive :exec
UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL;

-- name: UpdateWorkspaceForkMillis :exec
UPDATE workspaces SET fork_millis = ? WHERE id = ?;

-- name: GetWorkspace :one
SELECT session_id, node_id, fork_millis, released_at
FROM workspaces
WHERE id = ?;

-- name: ListLiveNodeWorkspaces :many
SELECT id, session_id, fork_millis
FROM workspaces
WHERE node_id = ? AND released_at IS NULL;

-- name: ListWorkspacesBySession :many
SELECT id, node_id, created_at, released_at
FROM workspaces
WHERE session_id = ?
ORDER BY created_at;

-- name: ListWorkspacesBySessions :many
SELECT id, session_id, node_id, created_at, released_at
FROM workspaces
WHERE session_id IN (sqlc.slice(session_ids))
ORDER BY created_at;

-- name: ListReleasedWorkspaceIDsIn :many
SELECT id
FROM workspaces
WHERE id IN (sqlc.slice(ids)) AND released_at IS NOT NULL;

-- name: ListLiveSessionWorkspaces :many
SELECT id, node_id
FROM workspaces
WHERE session_id = ? AND released_at IS NULL
ORDER BY created_at;

-- name: ListForksSince :many
SELECT created_at, CAST(COALESCE(fork_millis, -1) AS INTEGER) AS fork_millis
FROM workspaces
WHERE created_at >= ?
ORDER BY created_at;
