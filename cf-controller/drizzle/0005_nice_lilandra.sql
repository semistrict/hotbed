ALTER TABLE `samples` ADD `r2_up` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `samples` ADD `r2_down` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `samples` ADD `r2_puts` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `samples` ADD `r2_gets` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `samples` ADD `r2_fails` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `samples` ADD `r2_wait_ns` integer DEFAULT 0 NOT NULL;