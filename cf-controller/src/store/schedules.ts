import {
	ScheduledCommandSchema,
	ScheduledRunSchema,
	type ScheduledCommand,
	type ScheduledRun,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { clone } from "@bufbuild/protobuf";
import { marshal, unmarshal, optBig, type Row, type SqlKernel } from "./kernel.ts";
import type { ProjectStore } from "./projects.ts";

/** How many finished runs of each schedule are kept. */
export const KEEP_RUNS = 200;

function runRow(row: Row): ScheduledRun {
	const r = unmarshal(ScheduledRunSchema, str(row.doc));
	r.id = big(row.id);
	return r;
}

/** Scheduled commands and their runs. */
export class ScheduleStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
	) {}

	countByProject(): Map<string, number> {
		const out = new Map<string, number>();
		for (const r of this.sql
			.exec(
				"SELECT p.name, COUNT(*) AS cnt FROM schedules s JOIN projects p ON p.id = s.project_id GROUP BY p.name",
			)
			.toArray()) {
			out.set(str(r.name), num(r.cnt));
		}
		return out;
	}

	put(sc: ScheduledCommand): void {
		const pid = this.projects.idByName(sc.project);
		const stored = clone(ScheduledCommandSchema, sc);
		stored.project = "";
		stored.nextAt = undefined;
		stored.runs = [];
		stored.reminder = undefined;
		this.sql.exec(
			`INSERT INTO schedules (name, project_id, doc, next_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET project_id = excluded.project_id, doc = excluded.doc, next_at = excluded.next_at`,
			sc.name,
			pid,
			marshal(ScheduledCommandSchema, stored),
			sc.nextAt !== undefined ? Number(sc.nextAt) : null,
		);
	}

	get(name: string): ScheduledCommand {
		const rows = this.sql
			.exec(
				"SELECT s.doc, s.next_at, p.name AS project FROM schedules s JOIN projects p ON p.id = s.project_id WHERE s.name = ?",
				name,
			)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no scheduled command ${name}`).withSuggestion(
				"hotbed schedule ls",
			);
		}
		const sc = unmarshal(ScheduledCommandSchema, str(rows[0].doc));
		sc.project = str(rows[0].project);
		sc.nextAt = optBig(rows[0].next_at);
		return sc;
	}

	list(project: string): ScheduledCommand[] {
		return this.sql
			.exec(
				`SELECT s.doc, s.next_at, p.name AS project
				 FROM schedules s JOIN projects p ON p.id = s.project_id
				 WHERE (? = '' OR p.name = ?)
				 ORDER BY s.name`,
				project,
				project,
			)
			.toArray()
			.map((r) => {
				const sc = unmarshal(ScheduledCommandSchema, str(r.doc));
				sc.project = str(r.project);
				sc.nextAt = optBig(r.next_at);
				return sc;
			});
	}

	delete(name: string): boolean {
		return this.sql.exec("DELETE FROM schedules WHERE name = ?", name).rowsWritten > 0;
	}

	setNextAt(name: string, nextAt: bigint | undefined): void {
		this.sql.exec(
			"UPDATE schedules SET next_at = ? WHERE name = ?",
			nextAt !== undefined ? Number(nextAt) : null,
			name,
		);
	}

	addRun(run: ScheduledRun): ScheduledRun {
		const stored = clone(ScheduledRunSchema, run);
		stored.id = 0n;
		const id = this.sql.insert(
			"INSERT INTO schedule_runs (schedule, session_id, command_id, ended_at, doc) VALUES (?, ?, ?, ?, ?)",
			run.schedule,
			run.sessionId,
			run.commandId,
			run.endedAt !== undefined ? Number(run.endedAt) : null,
			marshal(ScheduledRunSchema, stored),
		);
		run.id = BigInt(id);
		return run;
	}

	updateLiveRun(run: ScheduledRun): boolean {
		const stored = clone(ScheduledRunSchema, run);
		stored.id = 0n;
		const res = this.sql.exec(
			"UPDATE schedule_runs SET session_id = ?, command_id = ?, ended_at = ?, doc = ? WHERE id = ? AND ended_at IS NULL",
			run.sessionId,
			run.commandId,
			run.endedAt !== undefined ? Number(run.endedAt) : null,
			marshal(ScheduledRunSchema, stored),
			run.id,
		);
		return res.rowsWritten > 0;
	}

	getRun(id: bigint): ScheduledRun {
		const rows = this.sql
			.exec("SELECT id, doc FROM schedule_runs WHERE id = ?", Number(id))
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no scheduled run ${id}`);
		return runRow(rows[0]);
	}

	liveRun(schedule: string): ScheduledRun | null {
		const rows = this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE schedule = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1",
				schedule,
			)
			.toArray();
		if (rows.length === 0) return null;
		return runRow(rows[0]);
	}

	allLiveRuns(): ScheduledRun[] {
		return this.sql
			.exec("SELECT id, doc FROM schedule_runs WHERE ended_at IS NULL ORDER BY id")
			.toArray()
			.map(runRow);
	}

	liveRunsOfSession(sessionId: string): ScheduledRun[] {
		return this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE session_id = ? AND ended_at IS NULL",
				sessionId,
			)
			.toArray()
			.map(runRow);
	}

	runs(schedule: string, limit: number): ScheduledRun[] {
		return this.sql
			.exec(
				"SELECT id, doc FROM schedule_runs WHERE schedule = ? ORDER BY id DESC LIMIT ?",
				schedule,
				limit,
			)
			.toArray()
			.map(runRow);
	}

	recentRuns(project: string, perSchedule: number): Map<string, ScheduledRun[]> {
		const out = new Map<string, ScheduledRun[]>();
		const rows = this.sql
			.exec(
				`SELECT r.id, r.schedule, r.doc FROM (
					SELECT sr.id, sr.schedule, sr.doc,
					       ROW_NUMBER() OVER (PARTITION BY sr.schedule ORDER BY sr.id DESC) AS rn
					FROM schedule_runs sr
					JOIN schedules s ON s.name = sr.schedule
					JOIN projects p ON p.id = s.project_id
					WHERE (? = '' OR p.name = ?)
				) r WHERE r.rn <= ? ORDER BY r.schedule, r.id DESC`,
				project,
				project,
				perSchedule,
			)
			.toArray();
		for (const row of rows) {
			const sched = str(row.schedule);
			const run = runRow(row);
			let list = out.get(sched);
			if (!list) {
				list = [];
				out.set(sched, list);
			}
			list.push(run);
		}
		return out;
	}

	/** Drops finished runs past each schedule's newest KEEP_RUNS, and those of deleted schedules. */
	prune(): void {
		this.sql.exec(
			`DELETE FROM schedule_runs WHERE ended_at IS NOT NULL AND (
				schedule NOT IN (SELECT name FROM schedules) OR
				id NOT IN (SELECT id FROM schedule_runs r WHERE r.schedule = schedule_runs.schedule ORDER BY id DESC LIMIT ?)
			)`,
			KEEP_RUNS,
		);
	}
}
