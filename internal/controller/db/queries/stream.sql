-- name: UpsertNodeState :exec
INSERT INTO node_state (node_id, kind, id, doc)
VALUES (?, ?, ?, ?)
ON CONFLICT (node_id, kind, id) DO UPDATE SET doc = excluded.doc;

-- name: DeleteNodeState :exec
DELETE FROM node_state WHERE node_id = ? AND kind = ? AND id = ?;

-- name: DeleteAllNodeStateForNode :exec
DELETE FROM node_state WHERE node_id = ?;

-- name: ListNodeStateForNode :many
SELECT node_id, kind, doc
FROM node_state
WHERE node_id = ?
ORDER BY node_id, kind, id;

-- name: ListAllNodeState :many
SELECT node_id, kind, doc
FROM node_state
ORDER BY node_id, kind, id;

-- name: GetCommandStateDoc :one
SELECT node_id, doc
FROM node_state
WHERE kind = 'command' AND id = ?
ORDER BY node_id
LIMIT 1;

-- name: GetBaseByCommand :one
SELECT node_id, doc
FROM node_state
WHERE kind = 'base' AND json_extract(doc, '$.command') = ?
ORDER BY node_id, id
LIMIT 1;

-- name: CountNodeLiveCommands :one
SELECT
	CAST(COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') = 'COMMAND_STATE_RUNNING' THEN 1 ELSE 0 END), 0) AS INTEGER) AS running,
	CAST(COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') = 'COMMAND_STATE_QUEUED' THEN 1 ELSE 0 END), 0) AS INTEGER) AS queued,
	CAST(COALESCE(SUM(CASE WHEN json_extract(doc, '$.state') IN ('COMMAND_STATE_PARKED', 'COMMAND_STATE_PAUSED') THEN 1 ELSE 0 END), 0) AS INTEGER) AS parked
FROM node_state
WHERE node_id = ? AND kind = 'command';

-- name: ListWorkspacesToRelease :many
SELECT s.id
FROM node_state s
JOIN workspaces w ON w.id = s.id
LEFT JOIN sessions sess ON sess.id = w.session_id
WHERE s.node_id = ?
  AND s.kind = 'workspace'
  AND (w.released_at IS NOT NULL OR sess.ended_at IS NOT NULL)
  AND json_extract(s.doc, '$.state') IS NOT 'WORKSPACE_STATE_RELEASED'
ORDER BY s.id;
