-- +goose Up
-- Priority preemption (parking lower-priority work to make room) is gone;
-- drop the records that named who preempted whom.
DELETE FROM records WHERE kind = 'preemption';

-- +goose Down
SELECT 1;
