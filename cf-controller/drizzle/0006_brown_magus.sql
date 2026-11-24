CREATE TABLE `worker_configs` (
	`scope` text PRIMARY KEY NOT NULL,
	`revision` integer DEFAULT 1 NOT NULL,
	`doc` text NOT NULL,
	`updated_at` integer NOT NULL,
	`updated_by` text DEFAULT '' NOT NULL
);
