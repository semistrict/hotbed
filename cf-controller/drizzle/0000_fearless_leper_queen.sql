CREATE TABLE `command_diagnostics` (
	`command_id` text NOT NULL,
	`seq` integer NOT NULL,
	`node_id` text NOT NULL,
	`created_at` integer NOT NULL,
	`doc` text NOT NULL,
	PRIMARY KEY(`command_id`, `seq`)
);
--> statement-breakpoint
CREATE INDEX `command_diagnostics_created` ON `command_diagnostics` (`created_at`);--> statement-breakpoint
CREATE TABLE `command_profiles` (
	`id` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`ended_at` integer NOT NULL,
	`doc` blob NOT NULL
);
--> statement-breakpoint
CREATE INDEX `command_profiles_ended` ON `command_profiles` (`ended_at`);--> statement-breakpoint
CREATE TABLE `commands` (
	`id` text PRIMARY KEY NOT NULL,
	`session_id` text NOT NULL,
	`node_id` text NOT NULL,
	`workspace_id` text NOT NULL,
	`created_at` integer NOT NULL
);
--> statement-breakpoint
CREATE INDEX `commands_session` ON `commands` (`session_id`);--> statement-breakpoint
CREATE INDEX `commands_created` ON `commands` (`created_at`);--> statement-breakpoint
CREATE TABLE `controller_migration` (
	`id` integer PRIMARY KEY NOT NULL,
	`target_controller` text NOT NULL,
	`controller_node_id` text DEFAULT '' NOT NULL,
	`default_token` text DEFAULT '' NOT NULL,
	`worker_tokens_json` text DEFAULT '{}' NOT NULL,
	`migrated_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `daemon_logs` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`at` integer NOT NULL,
	`level` integer NOT NULL,
	`msg` text NOT NULL,
	`attrs_json` text DEFAULT '' NOT NULL,
	`source_loc` text DEFAULT '' NOT NULL
);
--> statement-breakpoint
CREATE INDEX `daemon_logs_at` ON `daemon_logs` (`at`);--> statement-breakpoint
CREATE TABLE `events` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`at` integer NOT NULL,
	`kind` text NOT NULL,
	`node_id` text NOT NULL,
	`text` text NOT NULL,
	`ref_type` text NOT NULL,
	`ref_id` text NOT NULL
);
--> statement-breakpoint
CREATE TABLE `git_credentials` (
	`name` text PRIMARY KEY NOT NULL,
	`host` text NOT NULL,
	`doc` text NOT NULL,
	`secret` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `git_credentials_host` ON `git_credentials` (`host`);--> statement-breakpoint
CREATE TABLE `history` (
	`id` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`repo_id` text NOT NULL,
	`ended_at` integer NOT NULL,
	`doc` text NOT NULL,
	`project_id` integer,
	`session_id` text DEFAULT '' NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `history_ended` ON `history` (`ended_at`);--> statement-breakpoint
CREATE INDEX `history_project` ON `history` (`project_id`,`ended_at`);--> statement-breakpoint
CREATE INDEX `history_session` ON `history` (`session_id`,`ended_at`);--> statement-breakpoint
CREATE TABLE `kv_meta` (
	`key` text PRIMARY KEY NOT NULL,
	`value` text NOT NULL
);
--> statement-breakpoint
CREATE TABLE `node_state` (
	`node_id` text NOT NULL,
	`kind` text NOT NULL,
	`id` text NOT NULL,
	`doc` text NOT NULL,
	PRIMARY KEY(`node_id`, `kind`, `id`)
);
--> statement-breakpoint
CREATE TABLE `nodes` (
	`id` text PRIMARY KEY NOT NULL,
	`hostname` text NOT NULL,
	`login` text NOT NULL,
	`addr` text DEFAULT '' NOT NULL,
	`joined_at` integer NOT NULL,
	`last_seen` integer,
	`epoch` text DEFAULT '' NOT NULL,
	`applied` integer DEFAULT 0 NOT NULL,
	`applied_at` integer DEFAULT 0 NOT NULL,
	`heartbeat` text,
	`retired_at` integer
);
--> statement-breakpoint
CREATE TABLE `placements` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`at` integer NOT NULL,
	`session_id` text NOT NULL,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `placements_at` ON `placements` (`at`);--> statement-breakpoint
CREATE TABLE `project_changes` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer NOT NULL,
	`what` text NOT NULL,
	`repo` text NOT NULL,
	`at` integer NOT NULL,
	`session` text NOT NULL,
	`host` text NOT NULL,
	`reason` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `project_changes_project` ON `project_changes` (`project_id`,`seq`);--> statement-breakpoint
CREATE TABLE `project_repos` (
	`project_id` integer NOT NULL,
	`remote` text NOT NULL,
	PRIMARY KEY(`project_id`, `remote`),
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `project_repos_remote` ON `project_repos` (`remote`);--> statement-breakpoint
CREATE TABLE `projects` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`name` text NOT NULL,
	`created_at` integer NOT NULL,
	`setup` text DEFAULT '{}' NOT NULL
);
--> statement-breakpoint
CREATE UNIQUE INDEX `projects_name_unique` ON `projects` (`name`);--> statement-breakpoint
CREATE TABLE `question_changes` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`question_id` integer NOT NULL,
	`what` text NOT NULL,
	`at` integer NOT NULL,
	`session` text NOT NULL,
	`host` text NOT NULL,
	`reason` text NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`question_id`) REFERENCES `questions`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `question_changes_question` ON `question_changes` (`question_id`,`seq`);--> statement-breakpoint
CREATE TABLE `questions` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer NOT NULL,
	`repo` text NOT NULL,
	`state` text NOT NULL,
	`rank` integer NOT NULL,
	`asked_by` text NOT NULL,
	`asked_at` integer NOT NULL,
	`updated_at` integer NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `questions_project` ON `questions` (`project_id`,`state`);--> statement-breakpoint
CREATE TABLE `repos` (
	`id` text PRIMARY KEY NOT NULL,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE TABLE `review_events` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`review_id` integer NOT NULL,
	`finding_id` integer NOT NULL,
	`what` text NOT NULL,
	`at` integer NOT NULL,
	`session` text NOT NULL,
	`host` text NOT NULL,
	`quick` text NOT NULL,
	`text` text NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`review_id`) REFERENCES `reviews`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `review_events_review` ON `review_events` (`review_id`,`seq`);--> statement-breakpoint
CREATE TABLE `review_findings` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`review_id` integer NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`review_id`) REFERENCES `reviews`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `review_findings_review` ON `review_findings` (`review_id`,`id`);--> statement-breakpoint
CREATE TABLE `reviews` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`project_id` integer NOT NULL,
	`state` text NOT NULL,
	`requested_by` text NOT NULL,
	`requested_at` integer NOT NULL,
	`updated_at` integer NOT NULL,
	`doc` text NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `reviews_project` ON `reviews` (`project_id`,`state`);--> statement-breakpoint
CREATE TABLE `samples` (
	`node_id` text NOT NULL,
	`t` integer NOT NULL,
	`cpu` real NOT NULL,
	`memory` real NOT NULL,
	`psi` real NOT NULL,
	`cpu_psi` real DEFAULT 0 NOT NULL,
	`holding` integer DEFAULT 0 NOT NULL,
	`running` integer NOT NULL,
	`queued` integer NOT NULL,
	`parked` integer NOT NULL,
	`oom` integer NOT NULL,
	PRIMARY KEY(`node_id`, `t`)
);
--> statement-breakpoint
CREATE INDEX `samples_t` ON `samples` (`t`);--> statement-breakpoint
CREATE TABLE `schedule_runs` (
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`schedule` text NOT NULL,
	`session_id` text DEFAULT '' NOT NULL,
	`command_id` text DEFAULT '' NOT NULL,
	`ended_at` integer,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `schedule_runs_schedule` ON `schedule_runs` (`schedule`,`id`);--> statement-breakpoint
CREATE INDEX `schedule_runs_session` ON `schedule_runs` (`session_id`);--> statement-breakpoint
CREATE INDEX `schedule_runs_live` ON `schedule_runs` (`ended_at`) WHERE "schedule_runs"."ended_at" IS NULL;--> statement-breakpoint
CREATE TABLE `schedules` (
	`name` text PRIMARY KEY NOT NULL,
	`project_id` integer NOT NULL,
	`doc` text NOT NULL,
	`next_at` integer,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `schedules_project` ON `schedules` (`project_id`);--> statement-breakpoint
CREATE TABLE `session_events` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`session_id` text NOT NULL,
	`doc` text NOT NULL
);
--> statement-breakpoint
CREATE INDEX `session_events_session` ON `session_events` (`session_id`,`seq`);--> statement-breakpoint
CREATE TABLE `sessions` (
	`id` text PRIMARY KEY NOT NULL,
	`agent` text NOT NULL,
	`project_id` integer NOT NULL,
	`t3code_thread_id` text DEFAULT '' NOT NULL,
	`started_at` integer NOT NULL,
	`last_active_at` integer NOT NULL,
	`ended_at` integer,
	`end_reason` text DEFAULT '' NOT NULL,
	FOREIGN KEY (`project_id`) REFERENCES `projects`(`id`) ON UPDATE no action ON DELETE no action
);
--> statement-breakpoint
CREATE INDEX `sessions_project` ON `sessions` (`project_id`,`ended_at`);--> statement-breakpoint
CREATE INDEX `sessions_t3code_thread_id` ON `sessions` (`t3code_thread_id`,`last_active_at`) WHERE "sessions"."t3code_thread_id" != '';--> statement-breakpoint
CREATE TABLE `worker_tokens` (
	`token_hash` text PRIMARY KEY NOT NULL,
	`node_id` text NOT NULL,
	`hostname` text NOT NULL,
	`created_at` integer NOT NULL
);
--> statement-breakpoint
CREATE INDEX `worker_tokens_node` ON `worker_tokens` (`node_id`);--> statement-breakpoint
CREATE TABLE `workspaces` (
	`id` text PRIMARY KEY NOT NULL,
	`session_id` text NOT NULL,
	`node_id` text NOT NULL,
	`agent` text NOT NULL,
	`created_at` integer NOT NULL,
	`fork_millis` integer,
	`released_at` integer
);
--> statement-breakpoint
CREATE INDEX `workspaces_session` ON `workspaces` (`session_id`);--> statement-breakpoint
CREATE INDEX `workspaces_created` ON `workspaces` (`created_at`);