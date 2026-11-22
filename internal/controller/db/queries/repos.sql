-- name: UpsertRepo :exec
INSERT INTO repos (id, doc)
VALUES (?, ?)
ON CONFLICT (id) DO UPDATE SET doc = excluded.doc;

-- name: DeleteRepo :execrows
DELETE FROM repos WHERE id = ?;

-- name: ListRepos :many
SELECT doc FROM repos ORDER BY id;
