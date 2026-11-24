import { sql } from "drizzle-orm";
import { blob, index, integer, primaryKey, real, sqliteTable, text } from "drizzle-orm/sqlite-core";

/**
 * Drizzle SQLite schema for the single Durable Object controller (`ControllerDO`).
 * Mirrors internal/controller/db/migrations/00011..00021 plus Cloudflare-DO
 * tables (`kv_meta`, `worker_tokens`, `daemon_logs`, `controller_migration`).
 *
 * Run `pnpm -C cf-controller run db:generate` after editing this file; the
 * generated migrations in `drizzle/` auto-run inside `ControllerDO`'s
 * `blockConcurrencyWhile` startup hook.
 */

export const kvMeta = sqliteTable("kv_meta", {
	key: text("key").primaryKey(),
	value: text("value").notNull(),
});

export const controllerMigration = sqliteTable("controller_migration", {
	id: integer("id").primaryKey(),
	targetController: text("target_controller").notNull(),
	controllerNodeId: text("controller_node_id").notNull().default(""),
	defaultToken: text("default_token").notNull().default(""),
	workerTokensJson: text("worker_tokens_json").notNull().default("{}"),
	migratedAt: integer("migrated_at").notNull(),
});

export const workerTokens = sqliteTable(
	"worker_tokens",
	{
		tokenHash: text("token_hash").primaryKey(),
		nodeId: text("node_id").notNull(),
		hostname: text("hostname").notNull(),
		createdAt: integer("created_at").notNull(),
	},
	(t) => [index("worker_tokens_node").on(t.nodeId)],
);

export const daemonLogs = sqliteTable(
	"daemon_logs",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		at: integer("at").notNull(),
		level: integer("level").notNull(),
		msg: text("msg").notNull(),
		attrsJson: text("attrs_json").notNull().default(""),
		sourceLoc: text("source_loc").notNull().default(""),
	},
	(t) => [index("daemon_logs_at").on(t.at)],
);

export const nodes = sqliteTable("nodes", {
	id: text("id").primaryKey(),
	hostname: text("hostname").notNull(),
	login: text("login").notNull(),
	addr: text("addr").notNull().default(""),
	joinedAt: integer("joined_at").notNull(),
	lastSeen: integer("last_seen"),
	epoch: text("epoch").notNull().default(""),
	applied: integer("applied").notNull().default(0),
	appliedAt: integer("applied_at").notNull().default(0),
	heartbeat: text("heartbeat"),
	retiredAt: integer("retired_at"),
});

export const nodeState = sqliteTable(
	"node_state",
	{
		nodeId: text("node_id").notNull(),
		kind: text("kind").notNull(),
		id: text("id").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [primaryKey({ columns: [t.nodeId, t.kind, t.id] })],
);

export const repos = sqliteTable("repos", {
	id: text("id").primaryKey(),
	doc: text("doc").notNull(),
});

export const projects = sqliteTable("projects", {
	id: integer("id").primaryKey({ autoIncrement: true }),
	name: text("name").notNull().unique(),
	createdAt: integer("created_at").notNull(),
	setup: text("setup").notNull().default("{}"),
});

export const projectRepos = sqliteTable(
	"project_repos",
	{
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		remote: text("remote").notNull(),
	},
	(t) => [
		primaryKey({ columns: [t.projectId, t.remote] }),
		index("project_repos_remote").on(t.remote),
	],
);

export const projectChanges = sqliteTable(
	"project_changes",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		what: text("what").notNull(),
		repo: text("repo").notNull(),
		at: integer("at").notNull(),
		session: text("session").notNull(),
		host: text("host").notNull(),
		reason: text("reason").notNull(),
	},
	(t) => [index("project_changes_project").on(t.projectId, t.seq)],
);

export const sessions = sqliteTable(
	"sessions",
	{
		id: text("id").primaryKey(),
		agent: text("agent").notNull(),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		t3codeThreadId: text("t3code_thread_id").notNull().default(""),
		startedAt: integer("started_at").notNull(),
		lastActiveAt: integer("last_active_at").notNull(),
		endedAt: integer("ended_at"),
		endReason: text("end_reason").notNull().default(""),
	},
	(t) => [
		index("sessions_project").on(t.projectId, t.endedAt),
		index("sessions_t3code_thread_id")
			.on(t.t3codeThreadId, t.lastActiveAt)
			.where(sql`${t.t3codeThreadId} != ''`),
	],
);

export const workspaces = sqliteTable(
	"workspaces",
	{
		id: text("id").primaryKey(),
		sessionId: text("session_id").notNull(),
		nodeId: text("node_id").notNull(),
		agent: text("agent").notNull(),
		createdAt: integer("created_at").notNull(),
		forkMillis: integer("fork_millis"),
		releasedAt: integer("released_at"),
	},
	(t) => [index("workspaces_session").on(t.sessionId), index("workspaces_created").on(t.createdAt)],
);

export const commands = sqliteTable(
	"commands",
	{
		id: text("id").primaryKey(),
		sessionId: text("session_id").notNull(),
		nodeId: text("node_id").notNull(),
		workspaceId: text("workspace_id").notNull(),
		createdAt: integer("created_at").notNull(),
	},
	(t) => [index("commands_session").on(t.sessionId), index("commands_created").on(t.createdAt)],
);

export const sessionEvents = sqliteTable(
	"session_events",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		sessionId: text("session_id").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("session_events_session").on(t.sessionId, t.seq)],
);

export const history = sqliteTable(
	"history",
	{
		id: text("id").primaryKey(),
		nodeId: text("node_id").notNull(),
		repoId: text("repo_id").notNull(),
		endedAt: integer("ended_at").notNull(),
		doc: text("doc").notNull(),
		projectId: integer("project_id").references(() => projects.id),
		sessionId: text("session_id").notNull().default(""),
	},
	(t) => [
		index("history_ended").on(t.endedAt),
		index("history_project").on(t.projectId, t.endedAt),
		index("history_session").on(t.sessionId, t.endedAt),
	],
);

export const commandProfiles = sqliteTable(
	"command_profiles",
	{
		id: text("id").primaryKey(),
		nodeId: text("node_id").notNull(),
		endedAt: integer("ended_at").notNull(),
		doc: blob("doc", { mode: "buffer" }).notNull(),
	},
	(t) => [index("command_profiles_ended").on(t.endedAt)],
);

export const commandDiagnostics = sqliteTable(
	"command_diagnostics",
	{
		commandId: text("command_id").notNull(),
		seq: integer("seq").notNull(),
		nodeId: text("node_id").notNull(),
		createdAt: integer("created_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [
		primaryKey({ columns: [t.commandId, t.seq] }),
		index("command_diagnostics_created").on(t.createdAt),
	],
);

export const samples = sqliteTable(
	"samples",
	{
		nodeId: text("node_id").notNull(),
		t: integer("t").notNull(),
		cpu: real("cpu").notNull(),
		memory: real("memory").notNull(),
		psi: real("psi").notNull(),
		cpuPsi: real("cpu_psi").notNull().default(0),
		holding: integer("holding").notNull().default(0),
		running: integer("running").notNull(),
		queued: integer("queued").notNull(),
		parked: integer("parked").notNull(),
		oom: integer("oom").notNull(),
	},
	(t) => [primaryKey({ columns: [t.nodeId, t.t] }), index("samples_t").on(t.t)],
);

export const events = sqliteTable("events", {
	id: integer("id").primaryKey({ autoIncrement: true }),
	at: integer("at").notNull(),
	kind: text("kind").notNull(),
	nodeId: text("node_id").notNull(),
	text: text("text").notNull(),
	refType: text("ref_type").notNull(),
	refId: text("ref_id").notNull(),
});

export const placements = sqliteTable(
	"placements",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		at: integer("at").notNull(),
		sessionId: text("session_id").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("placements_at").on(t.at)],
);

export const schedules = sqliteTable(
	"schedules",
	{
		name: text("name").primaryKey(),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		doc: text("doc").notNull(),
		nextAt: integer("next_at"),
	},
	(t) => [index("schedules_project").on(t.projectId)],
);

export const scheduleRuns = sqliteTable(
	"schedule_runs",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		schedule: text("schedule").notNull(),
		sessionId: text("session_id").notNull().default(""),
		commandId: text("command_id").notNull().default(""),
		endedAt: integer("ended_at"),
		doc: text("doc").notNull(),
	},
	(t) => [
		index("schedule_runs_schedule").on(t.schedule, t.id),
		index("schedule_runs_session").on(t.sessionId),
		index("schedule_runs_live")
			.on(t.endedAt)
			.where(sql`${t.endedAt} IS NULL`),
	],
);

export const questions = sqliteTable(
	"questions",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		repo: text("repo").notNull(),
		state: text("state").notNull(),
		rank: integer("rank").notNull(),
		askedBy: text("asked_by").notNull(),
		askedAt: integer("asked_at").notNull(),
		updatedAt: integer("updated_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("questions_project").on(t.projectId, t.state)],
);

export const questionChanges = sqliteTable(
	"question_changes",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		questionId: integer("question_id")
			.notNull()
			.references(() => questions.id),
		what: text("what").notNull(),
		at: integer("at").notNull(),
		session: text("session").notNull(),
		host: text("host").notNull(),
		reason: text("reason").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("question_changes_question").on(t.questionId, t.seq)],
);

export const reviews = sqliteTable(
	"reviews",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		state: text("state").notNull(),
		requestedBy: text("requested_by").notNull(),
		requestedAt: integer("requested_at").notNull(),
		updatedAt: integer("updated_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("reviews_project").on(t.projectId, t.state)],
);

export const reviewFindings = sqliteTable(
	"review_findings",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		reviewId: integer("review_id")
			.notNull()
			.references(() => reviews.id),
		doc: text("doc").notNull(),
	},
	(t) => [index("review_findings_review").on(t.reviewId, t.id)],
);

export const reviewEvents = sqliteTable(
	"review_events",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		reviewId: integer("review_id")
			.notNull()
			.references(() => reviews.id),
		findingId: integer("finding_id").notNull(),
		what: text("what").notNull(),
		at: integer("at").notNull(),
		session: text("session").notNull(),
		host: text("host").notNull(),
		quick: text("quick").notNull(),
		text: text("text").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("review_events_review").on(t.reviewId, t.seq)],
);

export const gitCredentials = sqliteTable(
	"git_credentials",
	{
		name: text("name").primaryKey(),
		host: text("host").notNull(),
		doc: text("doc").notNull(),
		secret: text("secret").notNull(),
	},
	(t) => [index("git_credentials_host").on(t.host)],
);
