-- +goose Up
-- Drop the retired outbox table (board 7872 / 9388); every live worker's
-- outbox is empty and nothing reads or writes it.
DROP TABLE IF EXISTS outbox;

-- +goose Down
CREATE TABLE IF NOT EXISTS outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	doc  TEXT NOT NULL
) STRICT;
