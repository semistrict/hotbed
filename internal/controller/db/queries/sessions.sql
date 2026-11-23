-- name: InsertSession :exec
INSERT INTO sessions (id, agent, project_id, started_at, last_active_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT s.id, s.agent, p.name AS project, s.started_at, s.last_active_at, s.ended_at, s.end_reason
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.id = ?;

-- name: GetSessionProjectID :one
SELECT project_id FROM sessions WHERE id = ?;

-- name: ListSessionProjects :many
SELECT s.id, p.name AS project
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.ended_at IS NULL
   OR s.id IN (
     SELECT json_extract(ns.doc, '$.sessionId')
     FROM node_state ns
     WHERE ns.kind IN ('workspace', 'command')
       AND json_extract(ns.doc, '$.sessionId') IS NOT NULL
       AND json_extract(ns.doc, '$.sessionId') != ''
   );

-- name: ListSessionProjectsByIDs :many
SELECT s.id, p.name AS project
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.id IN (sqlc.slice(ids));

-- name: ListRecentSessions :many
SELECT s.id, s.agent, p.name AS project, s.started_at, s.last_active_at, s.ended_at, s.end_reason
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE (s.ended_at IS NULL OR s.ended_at >= CAST(sqlc.arg(since) AS INTEGER))
  AND p.name = COALESCE(NULLIF(CAST(sqlc.arg(project) AS TEXT), ''), p.name)
ORDER BY s.last_active_at DESC
LIMIT 500;

-- name: ListLiveSessions :many
SELECT s.id, s.agent, p.name AS project, s.started_at, s.last_active_at, s.ended_at, s.end_reason
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.ended_at IS NULL
ORDER BY s.started_at;

-- name: TouchSession :execrows
UPDATE sessions SET last_active_at = ? WHERE id = ? AND ended_at IS NULL;

-- name: TouchLiveSession :exec
UPDATE sessions SET last_active_at = ? WHERE id = ? AND ended_at IS NULL;

-- name: TouchLiveSessionMax :exec
UPDATE sessions SET last_active_at = MAX(last_active_at, CAST(sqlc.arg(at) AS INTEGER)) WHERE id = sqlc.arg(id) AND ended_at IS NULL;

-- name: EndSession :exec
UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL;

-- name: EndSessionIfIdle :execrows
UPDATE sessions
SET ended_at = ?, end_reason = ?
WHERE sessions.id = ?
  AND sessions.ended_at IS NULL
  AND sessions.last_active_at < ?
  AND sessions.id NOT IN (SELECT sr.session_id FROM schedule_runs sr WHERE sr.ended_at IS NULL);

-- name: ResumeSession :exec
UPDATE sessions SET ended_at = NULL, end_reason = '', last_active_at = ? WHERE id = ? AND ended_at IS NOT NULL;

-- name: ListIdleSessionIDs :many
SELECT id
FROM sessions
WHERE ended_at IS NULL
  AND last_active_at < ?
  AND id NOT IN (SELECT session_id FROM schedule_runs WHERE ended_at IS NULL);

-- name: IsSessionLive :one
SELECT CAST(EXISTS (SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL) AS INTEGER) AS live;

-- name: InsertSessionEvent :exec
INSERT INTO session_events (session_id, doc) VALUES (?, ?);

-- name: ListSessionEventsAfter :many
SELECT seq, doc
FROM session_events
WHERE session_id = ? AND seq > ?
ORDER BY seq;
