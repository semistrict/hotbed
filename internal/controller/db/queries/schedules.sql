-- name: UpsertSchedule :exec
INSERT INTO schedules (name, project_id, doc, next_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
	project_id = excluded.project_id,
	doc = excluded.doc,
	next_at = excluded.next_at;

-- name: UpdateScheduleNextAt :exec
UPDATE schedules SET next_at = ? WHERE name = ?;

-- name: DeleteSchedule :execrows
DELETE FROM schedules WHERE name = ?;

-- name: DeleteEndedRunsForSchedule :exec
DELETE FROM schedule_runs WHERE schedule = ? AND ended_at IS NOT NULL;

-- name: GetSchedule :one
SELECT p.name AS project, s.doc, s.next_at
FROM schedules s
JOIN projects p ON p.id = s.project_id
WHERE s.name = ?;

-- name: ListSchedules :many
SELECT p.name AS project, s.doc, s.next_at
FROM schedules s
JOIN projects p ON p.id = s.project_id
WHERE p.name = COALESCE(NULLIF(CAST(sqlc.arg(project) AS TEXT), ''), p.name)
ORDER BY s.name;

-- name: InsertScheduleRun :execresult
INSERT INTO schedule_runs (schedule, session_id, command_id, ended_at, doc)
VALUES (?, ?, ?, ?, ?);

-- name: UpdateLiveScheduleRun :execrows
UPDATE schedule_runs
SET session_id = ?, command_id = ?, ended_at = ?, doc = ?
WHERE id = ? AND ended_at IS NULL;

-- name: GetSessionScheduleStatus :one
SELECT schedule, CAST((ended_at IS NULL) AS INTEGER) AS running
FROM schedule_runs
WHERE session_id = ?
LIMIT 1;

-- name: ListLiveScheduleRunsBySession :many
SELECT id, doc
FROM schedule_runs
WHERE session_id = ? AND ended_at IS NULL;

-- name: ListScheduleRuns :many
SELECT id, doc
FROM schedule_runs
WHERE schedule = ?
ORDER BY id DESC
LIMIT ?;

-- name: ListAllLiveScheduleRuns :many
SELECT id, doc
FROM schedule_runs
WHERE ended_at IS NULL
ORDER BY id;

-- name: GetLiveScheduleRun :one
SELECT id, doc
FROM schedule_runs
WHERE schedule = ? AND ended_at IS NULL
ORDER BY id DESC
LIMIT 1;

-- name: PruneScheduleRuns :exec
DELETE FROM schedule_runs
WHERE ended_at IS NOT NULL AND (
	schedule NOT IN (SELECT name FROM schedules) OR
	id NOT IN (SELECT id FROM schedule_runs r WHERE r.schedule = schedule_runs.schedule ORDER BY id DESC LIMIT ?)
);
