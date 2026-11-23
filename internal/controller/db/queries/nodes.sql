-- name: UpsertNode :exec
INSERT INTO nodes (id, hostname, login, addr, joined_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	hostname = excluded.hostname,
	login = excluded.login,
	addr = excluded.addr,
	retired_at = NULL;

-- name: ListNodes :many
-- Every node not retired.
SELECT id, hostname, login, addr, joined_at, last_seen, heartbeat
FROM nodes
WHERE retired_at IS NULL
ORDER BY hostname, id;

-- name: RetireNode :exec
UPDATE nodes SET retired_at = ? WHERE id = ?;

-- name: GetNode :one
SELECT id, hostname, login, addr, joined_at, last_seen, heartbeat
FROM nodes
WHERE id = ?;

-- name: GetNodeStreamState :one
SELECT epoch, applied, applied_at FROM nodes WHERE id = ?;

-- name: UpdateNodeStreamState :exec
UPDATE nodes SET epoch = ?, applied = ?, applied_at = ? WHERE id = ?;

-- name: GetNodeApplied :one
SELECT applied FROM nodes WHERE id = ?;

-- name: NodeExists :one
SELECT id FROM nodes WHERE id = ?;

-- name: UpdateNodeHeartbeat :exec
UPDATE nodes SET last_seen = ?, heartbeat = ? WHERE id = ?;

-- name: UpsertSample :exec
INSERT OR REPLACE INTO samples (node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListSamplesSince :many
SELECT node_id, t, cpu, memory, psi, cpu_psi, holding, running, queued, parked, oom
FROM samples
WHERE t >= ?
ORDER BY t;

-- name: PruneSamples :exec
DELETE FROM samples WHERE t < ?;

-- name: InsertFeedEvent :exec
INSERT INTO events (at, kind, node_id, text, ref_type, ref_id)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListRecentEvents :many
SELECT id, at, kind, node_id, text, ref_type, ref_id
FROM events
ORDER BY id DESC
LIMIT ?;

-- name: PruneEvents :exec
DELETE FROM events WHERE id <= (SELECT MAX(e.id) FROM events e) - ?;
