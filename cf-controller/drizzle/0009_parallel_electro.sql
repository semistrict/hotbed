ALTER TABLE `review_events` ADD `by_doc` text DEFAULT '' NOT NULL;--> statement-breakpoint
ALTER TABLE `review_events` ADD `reviewer_id` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `sessions` ADD `model` text DEFAULT '' NOT NULL;