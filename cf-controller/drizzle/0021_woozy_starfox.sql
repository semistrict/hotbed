CREATE TABLE `board_actors` (
	`id` text PRIMARY KEY NOT NULL,
	`kind` text NOT NULL,
	`handle` text NOT NULL,
	`handle_lower` text NOT NULL,
	`name` text NOT NULL,
	`session_id` text DEFAULT '' NOT NULL,
	`updated_at` integer NOT NULL,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `board_actors_handle` ON `board_actors` (`handle_lower`);--> statement-breakpoint
CREATE TABLE `board_attachment_chunks` (
	`attachment_id` text NOT NULL,
	`chunk_index` integer NOT NULL,
	`data` blob NOT NULL,
	PRIMARY KEY(`attachment_id`, `chunk_index`),
	FOREIGN KEY (`attachment_id`) REFERENCES `board_attachments`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `board_attachments` (
	`id` text PRIMARY KEY NOT NULL,
	`name` text NOT NULL,
	`content_type` text NOT NULL,
	`size` integer NOT NULL,
	`uploader` text DEFAULT '' NOT NULL,
	`created_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `board_events` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`at` integer NOT NULL,
	`project_id` integer,
	`type` text NOT NULL,
	`message_id` integer,
	`thread_id` integer,
	`actor_id` text DEFAULT '' NOT NULL,
	`before_doc` text,
	`after_doc` text,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `board_events_project` ON `board_events` (`project_id`,`seq`);--> statement-breakpoint
CREATE INDEX `board_events_thread` ON `board_events` (`thread_id`,`seq`);--> statement-breakpoint
CREATE TABLE `board_import_needs` (
	`message_id` integer NOT NULL,
	`need_external_id` text NOT NULL,
	PRIMARY KEY(`message_id`, `need_external_id`),
	FOREIGN KEY (`message_id`) REFERENCES `board_messages`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `board_messages` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer NOT NULL,
	`external_id` text,
	`idempotency_key` text,
	`re` integer,
	`reply_to` integer,
	`by_actor` text NOT NULL,
	`by_principal` text NOT NULL,
	`by_profile` text,
	`text_lower` text DEFAULT '' NOT NULL,
	`deleted` integer DEFAULT 0 NOT NULL,
	`created_at` integer NOT NULL,
	`edited_at` integer,
	`replies_count` integer DEFAULT 0 NOT NULL,
	`last_activity_id` integer DEFAULT 0 NOT NULL,
	`last_activity_at` integer DEFAULT 0 NOT NULL,
	`version` integer DEFAULT 0 NOT NULL,
	`revision` integer DEFAULT 1 NOT NULL,
	`has_task` integer DEFAULT 0 NOT NULL,
	`task_state` text,
	`task_owner` text,
	`task_for` text,
	`task_lease_until` integer,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE UNIQUE INDEX `board_messages_external_id` ON `board_messages` (`external_id`) WHERE "board_messages"."external_id" IS NOT NULL;--> statement-breakpoint
CREATE UNIQUE INDEX `board_messages_idempotency` ON `board_messages` (`idempotency_key`) WHERE "board_messages"."idempotency_key" IS NOT NULL;--> statement-breakpoint
CREATE INDEX `board_messages_project_re` ON `board_messages` (`project_id`,`re`,`last_activity_at`,`id`);--> statement-breakpoint
CREATE INDEX `board_messages_re` ON `board_messages` (`re`,`id`);--> statement-breakpoint
CREATE INDEX `board_messages_by_actor` ON `board_messages` (`by_actor`,`id`);--> statement-breakpoint
CREATE INDEX `board_messages_by_principal` ON `board_messages` (`by_principal`,`id`);--> statement-breakpoint
CREATE INDEX `board_messages_tasks` ON `board_messages` (`project_id`,`has_task`,`task_state`,`id`);--> statement-breakpoint
CREATE INDEX `board_messages_task_lease` ON `board_messages` (`task_lease_until`) WHERE "board_messages"."task_state" = 'claimed' AND "board_messages"."task_lease_until" IS NOT NULL;--> statement-breakpoint
CREATE TABLE `board_schedules` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer NOT NULL,
	`re` integer,
	`state` text NOT NULL,
	`next_at` integer,
	`by_actor` text NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `board_schedules_due` ON `board_schedules` (`state`,`next_at`) WHERE "board_schedules"."state" = 'pending';--> statement-breakpoint
CREATE INDEX `board_schedules_re` ON `board_schedules` (`re`,`state`);--> statement-breakpoint
CREATE INDEX `board_schedules_project` ON `board_schedules` (`project_id`,`state`);--> statement-breakpoint
CREATE TABLE `board_schemas` (
	`id` text PRIMARY KEY NOT NULL,
	`name` text NOT NULL,
	`version` integer NOT NULL,
	`description` text DEFAULT '' NOT NULL,
	`schema_json` text NOT NULL,
	`by_actor` text DEFAULT '' NOT NULL,
	`created_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `board_task_batches` (
	`idempotency_key` text PRIMARY KEY NOT NULL,
	`request_digest` text NOT NULL,
	`project_id` integer NOT NULL,
	`task_ids_json` text NOT NULL,
	`created_at` integer NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE TABLE `board_task_claims` (
	`message_id` integer PRIMARY KEY NOT NULL,
	`epoch` integer NOT NULL,
	`token_hash` text NOT NULL,
	`lease_seconds` integer NOT NULL,
	`actor` text NOT NULL,
	`until` integer NOT NULL,
	FOREIGN KEY (`message_id`) REFERENCES `board_messages`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `board_terms` (
	`message_id` integer NOT NULL,
	`term` text NOT NULL,
	`value` text NOT NULL,
	PRIMARY KEY(`message_id`, `term`, `value`),
	FOREIGN KEY (`message_id`) REFERENCES `board_messages`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE INDEX `board_terms_lookup` ON `board_terms` (`term`,`value`,`message_id`);