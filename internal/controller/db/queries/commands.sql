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
INSERT OR IGNORE INTO history (id, node_id, repo_id, project_id, session_id, ended_at, doc)
VALUES (?, ?, ?, ?, ?, ?, ?);

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

-- name: GetCommandProfile :one
SELECT doc FROM command_profiles WHERE id = ?;

-- name: UpsertCommandProfile :exec
INSERT INTO command_profiles (id, node_id, ended_at, doc)
VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET ended_at = excluded.ended_at, doc = excluded.doc;

-- name: PruneCommandProfiles :exec
DELETE FROM command_profiles WHERE ended_at < ?;

-- name: InsertCommandDiagnostics :exec
INSERT INTO command_diagnostics (command_id, seq, node_id, created_at, doc)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (command_id, seq) DO UPDATE SET node_id = excluded.node_id, created_at = excluded.created_at, doc = excluded.doc;

-- name: ListCommandDiagnostics :many
SELECT doc FROM command_diagnostics WHERE command_id = ? ORDER BY seq;

-- name: GetCommandDiagnosticBySeq :one
SELECT doc FROM command_diagnostics WHERE command_id = ? AND seq = ?;

-- name: ListCommandDiagnosticsAfter :many
SELECT doc FROM command_diagnostics WHERE command_id = ? AND seq > ? ORDER BY seq;

-- name: HasCommandDiagnostics :one
SELECT CAST(EXISTS (SELECT 1 FROM command_diagnostics WHERE command_id = ?) AS INTEGER) AS present;

-- name: PruneCommandDiagnostics :exec
DELETE FROM command_diagnostics WHERE created_at < ?;

-- name: ListHistoryMatching :many
-- The newest commands the filters pick, at most lim of them (-1 for every
-- one); empty filters match every command.
SELECT h.doc, CAST(COALESCE(p.name, '') AS TEXT) AS project
FROM history h
LEFT JOIN projects p ON p.id = h.project_id
WHERE (CAST(sqlc.arg(project) AS TEXT) = '' OR p.name = sqlc.arg(project))
  AND (CAST(sqlc.arg(session_id) AS TEXT) = '' OR h.session_id = sqlc.arg(session_id))
  AND (CAST(sqlc.arg(match) AS TEXT) = '' OR instr(json_extract(h.doc, '$.argv'), sqlc.arg(match)) > 0)
ORDER BY h.ended_at DESC
LIMIT sqlc.arg(lim);

-- name: CountHistoryMatching :one
-- How many commands ListHistoryMatching's filters pick.
SELECT COUNT(*)
FROM history h
LEFT JOIN projects p ON p.id = h.project_id
WHERE (CAST(sqlc.arg(project) AS TEXT) = '' OR p.name = sqlc.arg(project))
  AND (CAST(sqlc.arg(session_id) AS TEXT) = '' OR h.session_id = sqlc.arg(session_id))
  AND (CAST(sqlc.arg(match) AS TEXT) = '' OR instr(json_extract(h.doc, '$.argv'), sqlc.arg(match)) > 0);

-- name: ListHistoryIDsIn :many
SELECT id FROM history WHERE id IN (sqlc.slice(ids));
