import { sql } from "drizzle-orm";
import {
	blob,
	index,
	integer,
	primaryKey,
	real,
	sqliteTable,
	text,
	uniqueIndex,
} from "drizzle-orm/sqlite-core";

/**
 * Drizzle SQLite schema for the single Durable Object controller (`ControllerDO`).
 *
 * Run `pnpm -C cf-controller run db:generate` after editing this file; the
 * generated migrations in `drizzle/` auto-run inside `ControllerDO`'s
 * `blockConcurrencyWhile` startup hook.
 */

export const kvMeta = sqliteTable("kv_meta", {
	key: text("key").primaryKey(),
	value: text("value").notNull(),
});

export const workerTokens = sqliteTable(
	"worker_tokens",
	{
		tokenHash: text("token_hash").primaryKey(),
		nodeId: text("node_id").notNull(),
		hostname: text("hostname").notNull(),
		createdAt: integer("created_at").notNull(),
		revokedAt: integer("revoked_at"),
	},
	(t) => [index("worker_tokens_node").on(t.nodeId)],
);

export const joinTokens = sqliteTable(
	"join_tokens",
	{
		id: text("id").primaryKey(),
		tokenHash: text("token_hash").notNull().unique(),
		label: text("label").notNull().default(""),
		createdBy: text("created_by").notNull().default(""),
		createdAt: integer("created_at").notNull(),
		expiresAt: integer("expires_at"),
		revokedAt: integer("revoked_at"),
	},
	(t) => [index("join_tokens_created").on(t.createdAt)],
);

export const apiTokens = sqliteTable(
	"api_tokens",
	{
		id: text("id").primaryKey(),
		tokenHash: text("token_hash").notNull().unique(),
		name: text("name").notNull(),
		userId: text("user_id").notNull(),
		email: text("email").notNull(),
		orgRole: text("org_role").notNull().default("member"),
		createdAt: integer("created_at").notNull(),
		lastUsedAt: integer("last_used_at"),
		expiresAt: integer("expires_at"),
		revokedAt: integer("revoked_at"),
	},
	(t) => [index("api_tokens_user").on(t.userId), index("api_tokens_created").on(t.createdAt)],
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
	// Unused: workers listen on no address (they dial the controller's
	// tunnel). Kept, empty, so no live Durable Object needs a migration.
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
		t3codeThreadUrl: text("t3code_thread_url").notNull().default(""),
		model: text("model").notNull().default(""),
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
		// A throwaway workspace (hotbed run --fork): never the session's
		// workspace on its node.
		throwaway: integer("throwaway", { mode: "boolean" }).notNull().default(false),
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
		r2Up: integer("r2_up").notNull().default(0),
		r2Down: integer("r2_down").notNull().default(0),
		r2Puts: integer("r2_puts").notNull().default(0),
		r2Gets: integer("r2_gets").notNull().default(0),
		r2Fails: integer("r2_fails").notNull().default(0),
		r2WaitNs: integer("r2_wait_ns").notNull().default(0),
		peerGiven: integer("peer_given").notNull().default(0),
		peerServed: integer("peer_served").notNull().default(0),
		peerGivenBytes: integer("peer_given_bytes").notNull().default(0),
		peerServedBytes: integer("peer_served_bytes").notNull().default(0),
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
		t3codeThreadId: text("t3code_thread_id").notNull().default(""),
		askedAt: integer("asked_at").notNull(),
		updatedAt: integer("updated_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [
		index("questions_project").on(t.projectId, t.state),
		index("questions_t3code_thread_id")
			.on(t.t3codeThreadId)
			.where(sql`${t.t3codeThreadId} != ''`),
	],
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

// Problems with hotbed itself (IssueService): doc is the Issue as protobuf
// JSON, its votes and state copied out to sort and filter by.
export const issues = sqliteTable(
	"issues",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		state: text("state").notNull(),
		votes: integer("votes").notNull(),
		updatedAt: integer("updated_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("issues_state").on(t.state, t.votes)],
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
		byDoc: text("by_doc").notNull().default(""),
		reviewerId: integer("reviewer_id").notNull().default(0),
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

/**
 * Project bases in the shared bucket, a cladefs cell each, as workers hold
 * them: one chain of bases per project, arch and runtime, which every node of
 * that arch forks its workspaces from and builds the next base on. `base` is
 * the SharedBase (protobuf JSON); `seen_at` is when a worker first reported
 * it, which orders a chain; `replaced_at` is when a newer base of its chain
 * was first reported (null for the newest); `node_id` is the node that
 * deletes the cell once it goes (its first holder, its builder, or a node that
 * took it over from a retired one); `unheld_since` is when the last worker
 * stopped holding it.
 */
export const sharedBases = sqliteTable(
	"shared_bases",
	{
		tenant: text("tenant").notNull(),
		cell: text("cell").notNull(),
		project: text("project").notNull(),
		arch: text("arch").notNull(),
		runtime: text("runtime").notNull(),
		seenAt: integer("seen_at").notNull(),
		replacedAt: integer("replaced_at"),
		base: text("base").notNull(),
		nodeId: text("node_id").notNull(),
		unheldSince: integer("unheld_since"),
	},
	(t) => [
		primaryKey({ columns: [t.tenant, t.cell] }),
		index("shared_bases_lineage").on(t.project, t.arch, t.runtime, t.tenant, t.replacedAt),
	],
);

/** Which worker holds which shared base: no base is deleted while held. */
export const sharedBaseHolders = sqliteTable(
	"shared_base_holders",
	{
		nodeId: text("node_id").notNull(),
		tenant: text("tenant").notNull(),
		cell: text("cell").notNull(),
	},
	(t) => [
		primaryKey({ columns: [t.nodeId, t.tenant, t.cell] }),
		index("shared_base_holders_cell").on(t.tenant, t.cell),
	],
);

export const cladefsRetiredIncarnations = sqliteTable("cladefs_retired_incarnations", {
	incarnation: text("incarnation").primaryKey(),
	nodeId: text("node_id").notNull(),
	hostname: text("hostname").notNull(),
	retiredAt: integer("retired_at").notNull(),
});

export const memories = sqliteTable(
	"memories",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id").references(() => projects.id),
		scopeKey: text("scope_key").notNull().default(""),
		repo: text("repo").notNull().default(""),
		slug: text("slug").notNull(),
		state: text("state").notNull(),
		rank: integer("rank").notNull().default(0),
		proposedBy: text("proposed_by").notNull().default(""),
		proposedAt: integer("proposed_at").notNull(),
		updatedAt: integer("updated_at").notNull(),
		searchText: text("search_text").notNull().default(""),
		doc: text("doc").notNull(),
	},
	(t) => [
		uniqueIndex("memories_approved_slug")
			.on(t.scopeKey, t.slug)
			.where(sql`${t.state} = 'MEMORY_STATE_APPROVED'`),
		index("memories_scope_state").on(t.projectId, t.state, t.rank, t.updatedAt),
		index("memories_state_updated").on(t.state, t.updatedAt, t.id),
	],
);

export const memoryChanges = sqliteTable(
	"memory_changes",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		memoryId: integer("memory_id")
			.notNull()
			.references(() => memories.id),
		what: text("what").notNull(),
		at: integer("at").notNull(),
		session: text("session").notNull(),
		host: text("host").notNull(),
		reason: text("reason").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("memory_changes_memory").on(t.memoryId, t.seq)],
);

export const skills = sqliteTable(
	"skills",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id").references(() => projects.id),
		scopeKey: text("scope_key").notNull().default(""),
		repo: text("repo").notNull().default(""),
		name: text("name").notNull(),
		state: text("state").notNull(),
		rank: integer("rank").notNull().default(0),
		createdBy: text("created_by").notNull().default(""),
		createdAt: integer("created_at").notNull(),
		updatedAt: integer("updated_at").notNull(),
		searchText: text("search_text").notNull().default(""),
		doc: text("doc").notNull(),
	},
	(t) => [
		uniqueIndex("skills_active_name")
			.on(t.scopeKey, t.name)
			.where(sql`${t.state} = 'SKILL_STATE_ACTIVE'`),
		index("skills_scope_state").on(t.projectId, t.state, t.rank, t.updatedAt),
		index("skills_state_updated").on(t.state, t.updatedAt, t.id),
	],
);

export const skillFiles = sqliteTable(
	"skill_files",
	{
		skillId: integer("skill_id")
			.notNull()
			.references(() => skills.id, { onDelete: "cascade" }),
		path: text("path").notNull(),
		size: integer("size").notNull(),
		digest: text("digest").notNull(),
		mimeType: text("mime_type").notNull(),
		binary: integer("binary", { mode: "boolean" }).notNull().default(false),
		chunkCount: integer("chunk_count").notNull().default(0),
	},
	(t) => [primaryKey({ columns: [t.skillId, t.path] })],
);

export const skillFileChunks = sqliteTable(
	"skill_file_chunks",
	{
		skillId: integer("skill_id")
			.notNull()
			.references(() => skills.id, { onDelete: "cascade" }),
		path: text("path").notNull(),
		chunkIndex: integer("chunk_index").notNull(),
		data: blob("data", { mode: "buffer" }).notNull(),
	},
	(t) => [primaryKey({ columns: [t.skillId, t.path, t.chunkIndex] })],
);

export const skillChanges = sqliteTable(
	"skill_changes",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		skillId: integer("skill_id")
			.notNull()
			.references(() => skills.id, { onDelete: "cascade" }),
		what: text("what").notNull(),
		at: integer("at").notNull(),
		session: text("session").notNull(),
		host: text("host").notNull(),
		reason: text("reason").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("skill_changes_skill").on(t.skillId, t.seq)],
);

export const workerConfigs = sqliteTable("worker_configs", {
	scope: text("scope").primaryKey(),
	revision: integer("revision").notNull().default(1),
	doc: text("doc").notNull(),
	updatedAt: integer("updated_at").notNull(),
	updatedBy: text("updated_by").notNull().default(""),
});

export const signupCodes = sqliteTable(
	"signup_codes",
	{
		code: text("code").primaryKey(),
		label: text("label").notNull().default(""),
		maxUses: integer("max_uses"),
		useCount: integer("use_count").notNull().default(0),
		createdAt: integer("created_at").notNull(),
		revokedAt: integer("revoked_at"),
	},
	(t) => [index("signup_codes_created").on(t.createdAt)],
);

export const approvedUsers = sqliteTable(
	"approved_users",
	{
		userId: text("user_id").primaryKey(),
		email: text("email").notNull().default(""),
		signupCode: text("signup_code").notNull().default(""),
		approvedAt: integer("approved_at").notNull(),
	},
	(t) => [index("approved_users_at").on(t.approvedAt)],
);

export const baseFreshens = sqliteTable(
	"base_freshens",
	{
		id: text("id").primaryKey(),
		nodeId: text("node_id").notNull(),
		baseId: text("base_id").notNull(),
		project: text("project").notNull(),
		runtime: text("runtime").notNull().default(""),
		state: text("state").notNull(),
		cold: integer("cold").notNull().default(0),
		createdAt: integer("created_at").notNull(),
		startedAt: integer("started_at"),
		endedAt: integer("ended_at"),
		doc: text("doc").notNull(),
	},
	(t) => [
		index("base_freshens_created").on(t.createdAt),
		index("base_freshens_project_created").on(t.project, t.createdAt),
		index("base_freshens_node_created").on(t.nodeId, t.createdAt),
		index("base_freshens_base").on(t.baseId, t.nodeId),
	],
);

// ---- Native message board & task queue (scoped by Hotbed projects) ----

export const boardMessages = sqliteTable(
	"board_messages",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		externalId: text("external_id"),
		idempotencyKey: text("idempotency_key"),
		re: integer("re"),
		replyTo: integer("reply_to"),
		byActor: text("by_actor").notNull(),
		byPrincipal: text("by_principal").notNull(),
		byProfile: text("by_profile"),
		textLower: text("text_lower").notNull().default(""),
		deleted: integer("deleted").notNull().default(0),
		createdAt: integer("created_at").notNull(),
		editedAt: integer("edited_at"),
		repliesCount: integer("replies_count").notNull().default(0),
		lastActivityId: integer("last_activity_id").notNull().default(0),
		lastActivityAt: integer("last_activity_at").notNull().default(0),
		version: integer("version").notNull().default(0),
		revision: integer("revision").notNull().default(1),
		hasTask: integer("has_task").notNull().default(0),
		taskState: text("task_state"),
		taskOwner: text("task_owner"),
		taskFor: text("task_for"),
		taskLeaseUntil: integer("task_lease_until"),
		doc: text("doc").notNull(),
	},
	(t) => [
		uniqueIndex("board_messages_external_id")
			.on(t.externalId)
			.where(sql`${t.externalId} IS NOT NULL`),
		uniqueIndex("board_messages_idempotency")
			.on(t.idempotencyKey)
			.where(sql`${t.idempotencyKey} IS NOT NULL`),
		index("board_messages_project_re").on(t.projectId, t.re, t.lastActivityAt, t.id),
		index("board_messages_re").on(t.re, t.id),
		index("board_messages_by_actor").on(t.byActor, t.id),
		index("board_messages_by_principal").on(t.byPrincipal, t.id),
		index("board_messages_tasks").on(t.projectId, t.hasTask, t.taskState, t.id),
		index("board_messages_task_lease")
			.on(t.taskLeaseUntil)
			.where(sql`${t.taskState} = 'claimed' AND ${t.taskLeaseUntil} IS NOT NULL`),
	],
);

/**
 * Inverted term index for board messages:
 * - labels: term = label key, value = label value
 * - mentions: term = '@', value = handle or actor ID
 * - attachments: term = '^', value = attachment ID
 * - task needs: term = '>needs', value = prerequisite message ID
 */
export const boardTerms = sqliteTable(
	"board_terms",
	{
		messageId: integer("message_id")
			.notNull()
			.references(() => boardMessages.id, { onDelete: "cascade" }),
		term: text("term").notNull(),
		value: text("value").notNull(),
	},
	(t) => [
		primaryKey({ columns: [t.messageId, t.term, t.value] }),
		index("board_terms_lookup").on(t.term, t.value, t.messageId),
	],
);

export const boardEvents = sqliteTable(
	"board_events",
	{
		seq: integer("seq").primaryKey({ autoIncrement: true }),
		at: integer("at").notNull(),
		projectId: integer("project_id").references(() => projects.id),
		type: text("type").notNull(),
		messageId: integer("message_id"),
		threadId: integer("thread_id"),
		actorId: text("actor_id").notNull().default(""),
		beforeDoc: text("before_doc"),
		afterDoc: text("after_doc"),
		doc: text("doc").notNull(),
	},
	(t) => [
		index("board_events_project").on(t.projectId, t.seq),
		index("board_events_thread").on(t.threadId, t.seq),
	],
);

export const boardActors = sqliteTable(
	"board_actors",
	{
		id: text("id").primaryKey(),
		kind: text("kind").notNull(),
		handle: text("handle").notNull(),
		handleLower: text("handle_lower").notNull(),
		name: text("name").notNull(),
		sessionId: text("session_id").notNull().default(""),
		updatedAt: integer("updated_at").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [index("board_actors_handle").on(t.handleLower)],
);

export const boardSchemas = sqliteTable("board_schemas", {
	id: text("id").primaryKey(),
	name: text("name").notNull(),
	version: integer("version").notNull(),
	description: text("description").notNull().default(""),
	schemaJson: text("schema_json").notNull(),
	byActor: text("by_actor").notNull().default(""),
	createdAt: integer("created_at").notNull(),
});

export const boardAttachments = sqliteTable("board_attachments", {
	id: text("id").primaryKey(),
	name: text("name").notNull(),
	contentType: text("content_type").notNull(),
	size: integer("size").notNull(),
	uploader: text("uploader").notNull().default(""),
	createdAt: integer("created_at").notNull(),
});

export const boardAttachmentChunks = sqliteTable(
	"board_attachment_chunks",
	{
		attachmentId: text("attachment_id")
			.notNull()
			.references(() => boardAttachments.id, { onDelete: "cascade" }),
		chunkIndex: integer("chunk_index").notNull(),
		data: blob("data", { mode: "buffer" }).notNull(),
	},
	(t) => [primaryKey({ columns: [t.attachmentId, t.chunkIndex] })],
);

export const boardSchedules = sqliteTable(
	"board_schedules",
	{
		id: integer("id").primaryKey({ autoIncrement: true }),
		projectId: integer("project_id")
			.notNull()
			.references(() => projects.id),
		re: integer("re"),
		state: text("state").notNull(),
		nextAt: integer("next_at"),
		byActor: text("by_actor").notNull(),
		doc: text("doc").notNull(),
	},
	(t) => [
		index("board_schedules_due")
			.on(t.state, t.nextAt)
			.where(sql`${t.state} = 'pending'`),
		index("board_schedules_re").on(t.re, t.state),
		index("board_schedules_project").on(t.projectId, t.state),
	],
);

export const boardTaskBatches = sqliteTable("board_task_batches", {
	idempotencyKey: text("idempotency_key").primaryKey(),
	requestDigest: text("request_digest").notNull(),
	projectId: integer("project_id")
		.notNull()
		.references(() => projects.id),
	taskIdsJson: text("task_ids_json").notNull(),
	createdAt: integer("created_at").notNull(),
});

export const boardImportNeeds = sqliteTable(
	"board_import_needs",
	{
		messageId: integer("message_id")
			.notNull()
			.references(() => boardMessages.id, { onDelete: "cascade" }),
		needExternalId: text("need_external_id").notNull(),
	},
	(t) => [primaryKey({ columns: [t.messageId, t.needExternalId] })],
);
