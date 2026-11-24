CREATE TABLE `cladefs_retired_incarnations` (
	`incarnation` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`hostname` text NOT NULL,
	`retired_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `shared_bases` (
	`project` text NOT NULL,
	`arch` text NOT NULL,
	`recipe` text DEFAULT '' NOT NULL,
	`base_id` text NOT NULL,
	`tenant` text NOT NULL,
	`cell` text NOT NULL,
	`version` text NOT NULL,
	`node_id` text NOT NULL,
	`updated_at` integer NOT NULL,
	PRIMARY KEY(`project`, `arch`, `recipe`)
);
--> statement-breakpoint
CREATE INDEX `shared_bases_project_arch` ON `shared_bases` (`project`,`arch`,`updated_at`);