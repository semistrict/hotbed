-- name: InsertPlacement :execlastid
INSERT INTO placements (at, session_id, doc) VALUES (?, ?, ?);

-- name: ListPlacementsSince :many
SELECT id, at, doc FROM placements WHERE at >= ? ORDER BY id DESC;

-- name: PrunePlacements :exec
DELETE FROM placements WHERE at < ?;
