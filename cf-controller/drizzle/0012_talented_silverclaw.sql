DROP INDEX `shared_bases_project_arch`;--> statement-breakpoint
ALTER TABLE `shared_bases` ADD `runtime` text DEFAULT '' NOT NULL;--> statement-breakpoint
CREATE INDEX `shared_bases_project_arch` ON `shared_bases` (`project`,`arch`,`runtime`,`updated_at`);