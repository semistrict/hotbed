-- name: InsertReview :execresult
INSERT INTO reviews (project_id, state, requested_by, requested_at, updated_at, doc)
VALUES (?, ?, ?, ?, ?, '');

-- name: UpdateReview :exec
UPDATE reviews
SET state = ?, updated_at = ?, doc = ?
WHERE id = ?;

-- name: TouchReview :exec
UPDATE reviews
SET updated_at = MAX(updated_at, CAST(sqlc.arg(at) AS INTEGER))
WHERE id = sqlc.arg(id);

-- name: GetReview :one
SELECT r.id, p.name AS project, r.doc
FROM reviews r
JOIN projects p ON p.id = r.project_id
WHERE r.id = ?;

-- name: ListReviews :many
SELECT r.id, p.name AS project, r.doc
FROM reviews r
JOIN projects p ON p.id = r.project_id
WHERE p.name = COALESCE(NULLIF(CAST(sqlc.arg(project) AS TEXT), ''), p.name)
ORDER BY r.requested_at DESC, r.id DESC
LIMIT CAST(sqlc.arg(max_rows) AS INTEGER);

-- name: ListReviewsByStates :many
SELECT r.id, p.name AS project, r.doc
FROM reviews r
JOIN projects p ON p.id = r.project_id
WHERE p.name = COALESCE(NULLIF(CAST(? AS TEXT), ''), p.name)
  AND r.state IN (sqlc.slice('states'))
ORDER BY r.requested_at DESC, r.id DESC
LIMIT ?;

-- name: InsertReviewFinding :execresult
INSERT INTO review_findings (review_id, doc) VALUES (?, '');

-- name: UpdateReviewFinding :exec
UPDATE review_findings SET doc = ? WHERE id = ?;

-- name: GetReviewFinding :one
SELECT doc FROM review_findings WHERE id = ? AND review_id = ?;

-- name: ListReviewFindings :many
SELECT id, doc FROM review_findings WHERE review_id = ? ORDER BY id;

-- name: InsertReviewEvent :execresult
INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListReviewEventsAfter :many
SELECT seq, finding_id, what, at, session, host, quick, text
FROM review_events
WHERE review_id = ? AND seq > ?
ORDER BY seq;

-- name: ListReviewResponsesAfter :many
SELECT seq, finding_id, what, at, session, host, quick, text
FROM review_events
WHERE review_id = ? AND seq > ? AND what = ?
ORDER BY seq;

-- name: CountActiveReviewsByProject :many
SELECT p.name, CAST(COUNT(*) AS INTEGER) AS count
FROM reviews r
JOIN projects p ON p.id = r.project_id
WHERE r.state IN (sqlc.arg(requested_state), sqlc.arg(in_progress_state))
GROUP BY p.name;

-- name: CountReviewsToDecideByProject :many
SELECT p.name, CAST(COUNT(DISTINCT r.id) AS INTEGER) AS count
FROM reviews r
JOIN projects p ON p.id = r.project_id
JOIN review_findings f ON f.review_id = r.id
WHERE r.state = sqlc.arg(done_state)
  AND json_extract(f.doc, '$.decision') = sqlc.arg(open_decision)
GROUP BY p.name;
