CREATE TABLE `command_samples` (
	`command_id` text NOT NULL,
	`seq` integer NOT NULL,
	`node_id` text NOT NULL,
	`created_at` integer NOT NULL,
	`meta` text NOT NULL,
	`doc` blob NOT NULL,
	PRIMARY KEY(`command_id`, `seq`)
);
--> statement-breakpoint
CREATE INDEX `command_samples_created` ON `command_samples` (`created_at`);