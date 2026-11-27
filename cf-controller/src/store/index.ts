// ControllerStore: ControllerDO's SQLite, one domain store per table group,
// each built over one SqlKernel. Callers reach a domain directly
// (store.sessions.get(id), store.questions.change(...)); a domain store takes
// the others it needs in its constructor, so the wiring below is the whole
// dependency graph.
import type { DrizzleSqliteDODatabase } from "drizzle-orm/durable-sqlite";
import type * as dbSchema from "../db/schema.ts";
import { CladefsStore } from "./cladefs.ts";
import { CommandStore } from "./commands.ts";
import { DaemonLogStore } from "./daemon-logs.ts";
import { FleetStore } from "./fleet.ts";
import { FreshenStore } from "./freshens.ts";
import { GitCredentialStore } from "./git-credentials.ts";
import { IssueStore } from "./issues.ts";
import { SqlKernel } from "./kernel.ts";
import { MemoryStore } from "./memories.ts";
import { MetaStore } from "./meta.ts";
import { NodeStateStore } from "./node-state.ts";
import { ProjectStore } from "./projects.ts";
import { QuestionStore } from "./questions.ts";
import { ReviewStore } from "./reviews.ts";
import { ScheduleStore } from "./schedules.ts";
import { SessionStore } from "./sessions.ts";
import { SkillStore } from "./skills.ts";
import { TokenStore } from "./tokens.ts";
import { WorkerConfigStore } from "./worker-configs.ts";
import { WorkspaceStore } from "./workspaces.ts";

export { KEEP_HISTORY_MS, type CommandRoute } from "./commands.ts";
export { KEEP_EVENTS, KEEP_PLACEMENTS_MS, type SampleRow } from "./fleet.ts";
export type { GitCredentialSecret, StoredGitCredential } from "./git-credentials.ts";
export { LOST_DETAIL, type NodeStateView, type PushResult } from "./node-state.ts";
export { sessionEndedError } from "./sessions.ts";
export type { ForkRow, WorkspaceRow } from "./workspaces.ts";
export { SqlKernel };

export class ControllerStore {
	readonly kernel: SqlKernel;
	readonly meta: MetaStore;
	readonly tokens: TokenStore;
	readonly daemonLogs: DaemonLogStore;
	readonly workerConfigs: WorkerConfigStore;
	readonly gitCredentials: GitCredentialStore;
	readonly issues: IssueStore;
	readonly cladefs: CladefsStore;
	readonly projects: ProjectStore;
	readonly workspaces: WorkspaceStore;
	readonly sessions: SessionStore;
	readonly freshens: FreshenStore;
	readonly commands: CommandStore;
	readonly fleet: FleetStore;
	readonly nodeState: NodeStateStore;
	readonly schedules: ScheduleStore;
	readonly questions: QuestionStore;
	readonly reviews: ReviewStore;
	readonly memories: MemoryStore;
	readonly skills: SkillStore;

	constructor(storage: DurableObjectStorage, db: DrizzleSqliteDODatabase<typeof dbSchema>) {
		const sql = new SqlKernel(storage, db);
		this.kernel = sql;
		this.meta = new MetaStore(sql);
		this.tokens = new TokenStore(sql, this.meta);
		this.daemonLogs = new DaemonLogStore(sql);
		this.workerConfigs = new WorkerConfigStore(sql);
		this.gitCredentials = new GitCredentialStore(sql);
		this.issues = new IssueStore(sql);
		this.cladefs = new CladefsStore(sql, this.meta);
		this.projects = new ProjectStore(sql);
		this.workspaces = new WorkspaceStore(sql);
		this.sessions = new SessionStore(sql, this.projects, this.workspaces);
		this.freshens = new FreshenStore(sql);
		this.commands = new CommandStore(sql, this.freshens);
		this.fleet = new FleetStore(sql, this.sessions);
		this.nodeState = new NodeStateStore(
			sql,
			this.commands,
			this.freshens,
			this.sessions,
			this.workspaces,
		);
		this.schedules = new ScheduleStore(sql, this.projects);
		this.questions = new QuestionStore(sql, this.projects, this.sessions);
		this.reviews = new ReviewStore(sql, this.projects, this.sessions);
		this.memories = new MemoryStore(sql, this.projects);
		this.skills = new SkillStore(sql, this.projects);
	}

	/**
	 * Drops what each domain keeps only for a while. Freshens go before
	 * commands: a command row is kept while a freshen still names it.
	 */
	prune(nowMs: number): void {
		this.fleet.prune(nowMs);
		this.freshens.prune(nowMs);
		this.commands.prune(nowMs);
		this.schedules.prune();
		this.daemonLogs.prune(nowMs);
		this.sessions.prune(nowMs);
		this.cladefs.prune(nowMs);
		this.meta.prune(nowMs);
	}
}
