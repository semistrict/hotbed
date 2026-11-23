-- +goose Up
-- When the node was retired (RetireNode): gone for good, kept for its
-- history. NULL while it is a node.
ALTER TABLE nodes ADD COLUMN retired_at INTEGER;

-- +goose Down
ALTER TABLE nodes DROP COLUMN retired_at;
