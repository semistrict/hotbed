CREATE TABLE `skill_changes` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`skill_id` integer NOT NULL,
	`what` text NOT NULL,
	`at` integer NOT NULL,
	`session` text NOT NULL,
	`host` text NOT NULL,
	`reason` text NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`skill_id`) REFERENCES `skills`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE INDEX `skill_changes_skill` ON `skill_changes` (`skill_id`,`seq`);--> statement-breakpoint
CREATE TABLE `skill_file_chunks` (
	`skill_id` integer NOT NULL,
	`path` text NOT NULL,
	`chunk_index` integer NOT NULL,
	`data` blob NOT NULL,
	PRIMARY KEY(`skill_id`, `path`, `chunk_index`),
	FOREIGN KEY (`skill_id`) REFERENCES `skills`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `skill_files` (
	`skill_id` integer NOT NULL,
	`path` text NOT NULL,
	`size` integer NOT NULL,
	`digest` text NOT NULL,
	`mime_type` text NOT NULL,
	`binary` integer DEFAULT false NOT NULL,
	`chunk_count` integer DEFAULT 0 NOT NULL,
	PRIMARY KEY(`skill_id`, `path`),
	FOREIGN KEY (`skill_id`) REFERENCES `skills`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `skills` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer,
	`scope_key` text DEFAULT '' NOT NULL,
	`repo` text DEFAULT '' NOT NULL,
	`name` text NOT NULL,
	`state` text NOT NULL,
	`rank` integer DEFAULT 0 NOT NULL,
	`created_by` text DEFAULT '' NOT NULL,
	`created_at` integer NOT NULL,
	`updated_at` integer NOT NULL,
	`search_text` text DEFAULT '' NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE UNIQUE INDEX `skills_active_name` ON `skills` (`scope_key`,`name`) WHERE "skills"."state" = 'SKILL_STATE_ACTIVE';--> statement-breakpoint
CREATE INDEX `skills_scope_state` ON `skills` (`project_id`,`state`,`rank`,`updated_at`);--> statement-breakpoint
CREATE INDEX `skills_state_updated` ON `skills` (`state`,`updated_at`,`id`);