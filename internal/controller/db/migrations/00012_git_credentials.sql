-- +goose Up
-- Centralized git credentials held on the controller. Metadata lives in doc
-- (protobuf JSON of GitCredential); secret material lives in secret so list
-- queries never touch secret bytes.
CREATE TABLE git_credentials (
	name   TEXT PRIMARY KEY,
	host   TEXT NOT NULL,
	doc    TEXT NOT NULL,
	secret TEXT NOT NULL
) STRICT;
CREATE INDEX git_credentials_host ON git_credentials (host);

-- +goose Down
DROP INDEX git_credentials_host;
DROP TABLE git_credentials;
