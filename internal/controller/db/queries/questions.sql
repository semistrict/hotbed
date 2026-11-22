-- name: GetMinQuestionRank :one
SELECT CAST(COALESCE(MIN(rank), 0) AS INTEGER) AS min_rank FROM questions;

-- name: GetMaxQuestionRank :one
SELECT CAST(COALESCE(MAX(rank), 0) AS INTEGER) AS max_rank FROM questions;

-- name: InsertQuestion :execresult
INSERT INTO questions (project_id, repo, state, rank, asked_by, asked_at, updated_at, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, '');

-- name: UpdateQuestion :exec
UPDATE questions
SET state = ?, rank = ?, updated_at = ?, doc = ?
WHERE id = ?;

-- name: ShiftQuestionRanks :exec
UPDATE questions SET rank = rank + 1 WHERE rank >= ? AND id != ?;

-- name: InsertQuestionChange :exec
INSERT INTO question_changes (question_id, what, at, session, host, reason, doc)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetQuestion :one
SELECT q.id, q.rank, p.name AS project, q.doc
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.id = ?;

-- name: CountQuestions :one
SELECT CAST(COUNT(*) AS INTEGER) AS total
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.state IN (sqlc.slice('states'))
  AND p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND q.asked_by = COALESCE(NULLIF(CAST(? AS TEXT), ''), q.asked_by);

-- name: ListQuestionsNewest :many
SELECT q.id, q.rank, p.name AS project, q.doc
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.state IN (sqlc.slice('states'))
  AND p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND q.asked_by = COALESCE(NULLIF(CAST(? AS TEXT), ''), q.asked_by)
ORDER BY q.asked_at DESC, q.id DESC
LIMIT ?;

-- name: ListQuestionsOldest :many
SELECT q.id, q.rank, p.name AS project, q.doc
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.state IN (sqlc.slice('states'))
  AND p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND q.asked_by = COALESCE(NULLIF(CAST(? AS TEXT), ''), q.asked_by)
ORDER BY q.asked_at, q.id
LIMIT ?;

-- name: ListQuestionsUpdated :many
SELECT q.id, q.rank, p.name AS project, q.doc
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.state IN (sqlc.slice('states'))
  AND p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND q.asked_by = COALESCE(NULLIF(CAST(? AS TEXT), ''), q.asked_by)
ORDER BY q.updated_at DESC, q.id DESC
LIMIT ?;

-- name: ListQuestionsRank :many
SELECT q.id, q.rank, p.name AS project, q.doc
FROM questions q
JOIN projects p ON p.id = q.project_id
WHERE q.state IN (sqlc.slice('states'))
  AND p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND q.asked_by = COALESCE(NULLIF(CAST(? AS TEXT), ''), q.asked_by)
ORDER BY q.rank, q.id
LIMIT ?;

-- name: ListQuestionChanges :many
SELECT seq, what, at, session, host, reason, doc
FROM question_changes
WHERE question_id = ?
ORDER BY seq;

-- name: ListQuestionProjects :many
SELECT
	p.name,
	CAST(SUM(q.state = sqlc.arg(open_state)) AS INTEGER) AS open_count,
	CAST(SUM(q.state IN (sqlc.arg(answered_state), sqlc.arg(dismissed_state))) AS INTEGER) AS outbox_count,
	CAST(COUNT(*) AS INTEGER) AS total
FROM questions q
JOIN projects p ON p.id = q.project_id
GROUP BY p.name
ORDER BY p.name;

-- name: CountOpenQuestions :one
SELECT CAST(COUNT(*) AS INTEGER) AS count
FROM questions
WHERE state = ?;
