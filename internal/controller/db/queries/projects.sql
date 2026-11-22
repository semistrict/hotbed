-- name: InsertProjectIgnore :execresult
INSERT INTO projects (name, created_at)
VALUES (?, ?)
ON CONFLICT (name) DO NOTHING;

-- name: InsertProjectRepoIgnore :execresult
INSERT INTO project_repos (project_id, remote)
VALUES (?, ?)
ON CONFLICT DO NOTHING;

-- name: DeleteProjectRepo :execresult
DELETE FROM project_repos
WHERE project_id = ? AND remote = ?;

-- name: InsertProjectChange :exec
INSERT INTO project_changes (project_id, what, repo, at, session, host, reason)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListProjectChanges :many
SELECT seq, what, repo, at, session, host, reason
FROM project_changes
WHERE project_id = ?
ORDER BY seq;

-- name: GetProjectByName :one
SELECT id, name, created_at FROM projects WHERE name = ?;

-- name: GetProjectIDByName :one
SELECT id FROM projects WHERE name = ?;

-- name: ListProjectNames :many
SELECT name FROM projects ORDER BY name;

-- name: ListProjectNamesHoldingRemote :many
SELECT p.name
FROM projects p
JOIN project_repos r ON r.project_id = p.id
WHERE r.remote = ?
ORDER BY p.name;

-- name: ListProjectRepos :many
SELECT remote
FROM project_repos
WHERE project_id = ?
ORDER BY remote;

-- name: CountLiveSessionsByProject :many
SELECT p.name, CAST(COUNT(*) AS INTEGER) AS count
FROM sessions s
JOIN projects p ON p.id = s.project_id
WHERE s.ended_at IS NULL
GROUP BY p.name;

-- name: CountSchedulesByProject :many
SELECT p.name, CAST(COUNT(*) AS INTEGER) AS count
FROM schedules s
JOIN projects p ON p.id = s.project_id
GROUP BY p.name;
