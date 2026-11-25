PRAGMA foreign_keys=OFF;--> statement-breakpoint
CREATE TABLE `__new_base_freshens` (
	`id` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`base_id` text NOT NULL,
	`project` text NOT NULL,
	`runtime` text DEFAULT '' NOT NULL,
	`state` text NOT NULL,
	`cold` integer DEFAULT 0 NOT NULL,
	`created_at` integer NOT NULL,
	`started_at` integer,
	`ended_at` integer,
	`doc` text NOT NULL
);
--> statement-breakpoint
INSERT INTO `__new_base_freshens`("id", "node_id", "base_id", "project", "runtime", "state", "cold", "created_at", "started_at", "ended_at", "doc") SELECT "id", "node_id", "base_id", "project", "runtime", "state", "cold", "created_at", "started_at", "ended_at", "doc" FROM `base_freshens`;--> statement-breakpoint
DROP TABLE `base_freshens`;--> statement-breakpoint
ALTER TABLE `__new_base_freshens` RENAME TO `base_freshens`;--> statement-breakpoint
PRAGMA foreign_keys=ON;--> statement-breakpoint
CREATE INDEX `base_freshens_created` ON `base_freshens` (`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_project_created` ON `base_freshens` (`project`,`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_node_created` ON `base_freshens` (`node_id`,`created_at`);--> statement-breakpoint
CREATE INDEX `base_freshens_base` ON `base_freshens` (`base_id`,`node_id`);