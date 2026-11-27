-- A board claim now lives whole in its message (task.claim: actor, until, leaseSeconds); keep each live claim's lease length before dropping the table.
UPDATE `board_messages` SET `doc` = json_set(`doc`, '$.task.claim.leaseSeconds', c.`lease_seconds`) FROM `board_task_claims` c WHERE c.`message_id` = `board_messages`.`id` AND json_extract(`doc`, '$.task.claim') IS NOT NULL;--> statement-breakpoint
DROP TABLE `board_task_claims`;
