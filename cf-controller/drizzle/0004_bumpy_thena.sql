CREATE TABLE `api_tokens` (
	`id` text PRIMARY KEY NOT NULL,
	`token_hash` text NOT NULL,
	`name` text NOT NULL,
	`user_id` text NOT NULL,
	`email` text NOT NULL,
	`org_role` text DEFAULT 'member' NOT NULL,
	`created_at` integer NOT NULL,
	`last_used_at` integer,
	`expires_at` integer,
	`revoked_at` integer
);
--> statement-breakpoint
CREATE UNIQUE INDEX `api_tokens_token_hash_unique` ON `api_tokens` (`token_hash`);--> statement-breakpoint
CREATE INDEX `api_tokens_user` ON `api_tokens` (`user_id`);--> statement-breakpoint
CREATE INDEX `api_tokens_created` ON `api_tokens` (`created_at`);--> statement-breakpoint
CREATE TABLE `join_tokens` (
	`id` text PRIMARY KEY NOT NULL,
	`token_hash` text NOT NULL,
	`label` text DEFAULT '' NOT NULL,
	`created_by` text DEFAULT '' NOT NULL,
	`created_at` integer NOT NULL,
	`expires_at` integer,
	`revoked_at` integer
);
--> statement-breakpoint
CREATE UNIQUE INDEX `join_tokens_token_hash_unique` ON `join_tokens` (`token_hash`);--> statement-breakpoint
CREATE INDEX `join_tokens_created` ON `join_tokens` (`created_at`);--> statement-breakpoint
ALTER TABLE `worker_tokens` ADD `revoked_at` integer;