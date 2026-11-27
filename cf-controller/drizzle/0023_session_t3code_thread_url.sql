-- The link T3 Code gives a session's thread, for the cockpit to open it by.
ALTER TABLE `sessions` ADD `t3code_thread_url` text DEFAULT '' NOT NULL;
