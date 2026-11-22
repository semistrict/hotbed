-- name: InsertCommand :exec
INSERT INTO commands (id, session_id, node_id, workspace_id, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: InsertCommandIgnore :exec
INSERT OR IGNORE INTO commands (id, session_id, node_id, workspace_id, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: DeleteCommand :exec
DELETE FROM commands WHERE id = ?;

-- name: GetCommand :one
SELECT session_id, node_id FROM commands WHERE id = ?;

-- name: PruneCommands :exec
DELETE FROM commands WHERE created_at < ?;

-- name: InsertHistoryIgnore :exec
INSERT OR IGNORE INTO history (id, node_id, repo_id, project_id, ended_at, doc)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetHistoryNode :one
SELECT node_id FROM history WHERE id = ?;

-- name: GetHistoryDoc :one
SELECT h.doc, CAST(COALESCE(p.name, '') AS TEXT) AS project
FROM history h
LEFT JOIN projects p ON p.id = h.project_id
WHERE h.id = ?;

-- name: ListHistorySince :many
SELECT h.doc, CAST(COALESCE(p.name, '') AS TEXT) AS project
FROM history h
LEFT JOIN projects p ON p.id = h.project_id
WHERE h.ended_at >= ?
ORDER BY h.ended_at;

-- name: PruneHistory :exec
DELETE FROM history WHERE ended_at < ?;
