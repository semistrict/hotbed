CREATE TABLE `approved_users` (
	`user_id` text PRIMARY KEY NOT NULL,
	`email` text DEFAULT '' NOT NULL,
	`signup_code` text DEFAULT '' NOT NULL,
	`approved_at` integer NOT NULL
);
--> statement-breakpoint
CREATE INDEX `approved_users_at` ON `approved_users` (`approved_at`);--> statement-breakpoint
CREATE TABLE `signup_codes` (
	`code` text PRIMARY KEY NOT NULL,
	`label` text DEFAULT '' NOT NULL,
	`max_uses` integer,
	`use_count` integer DEFAULT 0 NOT NULL,
	`created_at` integer NOT NULL,
	`revoked_at` integer
);
--> statement-breakpoint
CREATE INDEX `signup_codes_created` ON `signup_codes` (`created_at`);