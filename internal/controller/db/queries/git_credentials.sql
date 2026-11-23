-- name: UpsertGitCredential :exec
INSERT INTO git_credentials (name, host, doc, secret)
VALUES (?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
	host = excluded.host,
	doc = excluded.doc,
	secret = excluded.secret;

-- name: DeleteGitCredential :execrows
DELETE FROM git_credentials WHERE name = ?;

-- name: ListGitCredentials :many
SELECT doc FROM git_credentials ORDER BY name;

-- name: ListGitCredentialsByHost :many
SELECT doc, secret FROM git_credentials WHERE host = ? ORDER BY name;
