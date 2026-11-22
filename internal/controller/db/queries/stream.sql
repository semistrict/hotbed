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

-- name: ListWorkspacesToRelease :many
SELECT s.id
FROM node_state s
JOIN workspaces w ON w.id = s.id
WHERE s.node_id = ?
  AND s.kind = 'workspace'
  AND w.released_at IS NOT NULL
  AND json_extract(s.doc, '$.state') IS NOT 'WORKSPACE_STATE_RELEASED'
ORDER BY s.id;
