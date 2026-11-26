CREATE TABLE `shared_base_holders` (
	`node_id` text NOT NULL,
	`tenant` text NOT NULL,
	`cell` text NOT NULL,
	PRIMARY KEY(`node_id`, `tenant`, `cell`)
);
--> statement-breakpoint
CREATE INDEX `shared_base_holders_cell` ON `shared_base_holders` (`tenant`,`cell`);--> statement-breakpoint
CREATE TABLE `shared_bases` (
	`tenant` text NOT NULL,
	`cell` text NOT NULL,
	`project` text NOT NULL,
	`arch` text NOT NULL,
	`runtime` text NOT NULL,
	`seen_at` integer NOT NULL,
	`replaced_at` integer,
	`base` text NOT NULL,
	`node_id` text NOT NULL,
	`unheld_since` integer,
	PRIMARY KEY(`tenant`, `cell`)
);
--> statement-breakpoint
CREATE INDEX `shared_bases_lineage` ON `shared_bases` (`project`,`arch`,`runtime`,`tenant`,`replaced_at`);