CREATE TABLE `memories` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer,
	`scope_key` text DEFAULT '' NOT NULL,
	`repo` text DEFAULT '' NOT NULL,
	`slug` text NOT NULL,
	`mode` text NOT NULL,
	`state` text NOT NULL,
	`rank` integer DEFAULT 0 NOT NULL,
	`proposed_by` text DEFAULT '' NOT NULL,
	`proposed_at` integer NOT NULL,
	`updated_at` integer NOT NULL,
	`search_text` text DEFAULT '' NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE UNIQUE INDEX `memories_approved_slug` ON `memories` (`scope_key`,`slug`) WHERE "memories"."state" = 'MEMORY_STATE_APPROVED';--> statement-breakpoint
CREATE INDEX `memories_scope_state` ON `memories` (`project_id`,`state`,`mode`,`rank`,`updated_at`);--> statement-breakpoint
CREATE INDEX `memories_state_updated` ON `memories` (`state`,`updated_at`,`id`);--> statement-breakpoint
CREATE TABLE `memory_changes` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`memory_id` integer NOT NULL,
	`what` text NOT NULL,
	`at` integer NOT NULL,
	`session` text NOT NULL,
	`host` text NOT NULL,
	`reason` text NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`memory_id`) REFERENCES `memories`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `memory_changes_memory` ON `memory_changes` (`memory_id`,`seq`);