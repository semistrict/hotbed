CREATE TABLE `base_freshens` (
	`id` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`base_id` text NOT NULL,
	`project` text NOT NULL,
	`runtime` text DEFAULT 'chroot' NOT NULL,
	`state` text NOT NULL,
	`cold` integer DEFAULT 0 NOT NULL,
	`created_at` integer NOT NULL,
	`started_at` integer,
	`ended_at` integer,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `base_freshens_created` ON `base_freshens` (`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_project_created` ON `base_freshens` (`project`,`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_node_created` ON `base_freshens` (`node_id`,`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_base` ON `base_freshens` (`base_id`,`node_id`);