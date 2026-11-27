DROP INDEX `memories_scope_state`;--> statement-breakpoint
CREATE INDEX `memories_scope_state` ON `memories` (`project_id`,`state`,`rank`,`updated_at`);--> statement-breakpoint
ALTER TABLE `memories` DROP COLUMN `mode`;--> statement-breakpoint
-- Memories no longer have a mode: each is a summary (listed at session start)
-- and a body (read on demand). A former always-on rule had only its summary,
-- so its body takes the summary's text.
UPDATE `memories` SET `doc` = json_remove(
	CASE WHEN COALESCE(json_extract(`doc`, '$.content.body'), '') = ''
		THEN json_set(`doc`, '$.content.body', json_extract(`doc`, '$.content.summary'))
		ELSE `doc` END,
	'$.content.mode');
