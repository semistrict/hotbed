-- name: InsertWorkspace :exec
INSERT INTO workspaces (id, session_id, node_id, repo_id, agent, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: InsertWorkspaceIgnore :exec
INSERT OR IGNORE INTO workspaces (id, session_id, node_id, repo_id, agent, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = ?;

-- name: ReleaseWorkspace :exec
UPDATE workspaces SET released_at = ? WHERE id = ?;

-- name: ReleaseWorkspaceIfLive :exec
UPDATE workspaces SET released_at = ? WHERE id = ? AND released_at IS NULL;

-- name: UpdateWorkspaceForkMillis :exec
UPDATE workspaces SET fork_millis = ? WHERE id = ?;

-- name: GetWorkspace :one
SELECT session_id, node_id, repo_id, released_at
FROM workspaces
WHERE id = ?;

-- name: ListWorkspacesBySession :many
SELECT id, repo_id, node_id, created_at, released_at
FROM workspaces
WHERE session_id = ?
ORDER BY created_at;

-- name: GetLiveSessionWorkspaceID :one
SELECT id
FROM workspaces
WHERE session_id = ? AND repo_id = ? AND released_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: ListForksSince :many
SELECT created_at, CAST(COALESCE(fork_millis, -1) AS INTEGER) AS fork_millis
FROM workspaces
WHERE created_at >= ?
ORDER BY created_at;
