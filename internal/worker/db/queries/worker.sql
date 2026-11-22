-- name: InsertMetaIgnore :exec
INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?);

-- name: UpdateMeta :exec
UPDATE meta SET value = ? WHERE key = ?;

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: UpsertRecord :exec
INSERT INTO records (kind, id, doc) VALUES (?, ?, ?)
ON CONFLICT (kind, id) DO UPDATE SET doc = excluded.doc;

-- name: DeleteRecord :exec
DELETE FROM records WHERE kind = ? AND id = ?;

-- name: ListRecordsByKind :many
SELECT doc FROM records WHERE kind = ?;

-- name: InsertEvent :execresult
INSERT INTO events (at, doc) VALUES (?, ?);

-- name: ListEventsAfter :many
SELECT seq, doc FROM events WHERE seq > ? ORDER BY seq LIMIT ?;

-- name: GetOldestEventSeq :one
SELECT CAST(COALESCE(MIN(seq), sqlc.arg(fallback)) AS INTEGER) AS seq FROM events;

-- name: GetEventAt :one
SELECT CAST(COALESCE((SELECT at FROM events WHERE seq = ?), 0) AS INTEGER) AS at;

-- name: PruneEvents :exec
DELETE FROM events
WHERE (events.seq <= sqlc.arg(applied) AND events.at < sqlc.arg(before_at))
   OR events.seq <= (SELECT MAX(e.seq) FROM events e) - sqlc.arg(keep);

-- name: ListPendingOutbox :many
SELECT seq, kind, doc FROM outbox ORDER BY seq LIMIT ?;

-- name: AckOutbox :exec
DELETE FROM outbox WHERE seq <= ?;
