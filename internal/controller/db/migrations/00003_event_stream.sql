-- +goose Up
-- Workers push their events, which the controller folds into its view
-- of each node (node_state), instead of reporting their whole state; the
-- last reports become the views the events continue from.
ALTER TABLE nodes ADD COLUMN epoch TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN applied INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN applied_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN heartbeat TEXT;

CREATE TABLE node_state (
	node_id TEXT NOT NULL,
	kind    TEXT NOT NULL, -- base, workspace, command
	id      TEXT NOT NULL,
	doc     TEXT NOT NULL,
	PRIMARY KEY (node_id, kind, id)
) STRICT;

INSERT INTO node_state (node_id, kind, id, doc)
	SELECT n.id, 'base', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.bases') j WHERE n.report IS NOT NULL;
INSERT INTO node_state (node_id, kind, id, doc)
	SELECT n.id, 'workspace', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.workspaces') j WHERE n.report IS NOT NULL;
INSERT INTO node_state (node_id, kind, id, doc)
	SELECT n.id, 'command', json_extract(j.value, '$.id'), j.value FROM nodes n, json_each(n.report, '$.commands') j WHERE n.report IS NOT NULL;
-- A report's usage fields read as a heartbeat's.
UPDATE nodes SET heartbeat = report;
ALTER TABLE nodes DROP COLUMN report;

-- +goose Down
ALTER TABLE nodes ADD COLUMN report TEXT;
DROP TABLE node_state;
ALTER TABLE nodes DROP COLUMN heartbeat;
ALTER TABLE nodes DROP COLUMN applied_at;
ALTER TABLE nodes DROP COLUMN applied;
ALTER TABLE nodes DROP COLUMN epoch;
