CREATE TABLE `issues` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`state` text NOT NULL,
	`votes` integer NOT NULL,
	`updated_at` integer NOT NULL,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `issues_state` ON `issues` (`state`,`votes`);