import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create } from "@bufbuild/protobuf";
import {
	CreateWorkspaceRequestSchema,
	DaemonLogLevel,
	DeleteScheduleResponseSchema,
	ListSchedulesResponseSchema,
	PlacementSchema,
	RunCommandRequestSchema,
	RunOutcome,
	ScheduledCommandSchema,
	ScheduledRunSchema,
	ScheduleService,
	StopCommandRequestSchema,
	type ScheduledCommand,
	type ScheduledRun,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { setupStepsReminder, type ControllerContext } from "../context.ts";
import { formatInTimeZone, parseCron } from "../cron.ts";
import { ErrorCode, codeOf, fromUnknownError, hotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";
import type { WorkspaceHelpers } from "./workspaces.ts";

const SLOT_GRACE_MS = 5 * 60_000;
const RUNS_LISTED = 10;
const RUNS_SHOWN = 50;
const MAX_SCHEDULE_NAME = 30;
const SCHEDULE_NAME_RE = /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/;

export interface ScheduleRunner {
	tickSchedules(): Promise<void>;
	allSchedules(project: string): ScheduledCommand[];
}

export function createScheduleService(
	ctrl: ControllerContext,
	ws: WorkspaceHelpers,
): { impl: ServiceImpl<typeof ScheduleService>; runner: ScheduleRunner } {
	function planNext(sc: ScheduledCommand): void {
		sc.nextAt = undefined;
		if (sc.paused) return;
		const parsed = parseCron(sc.cron, ctrl.now());
		const next = parsed.next(ctrl.now());
		if (next !== null) sc.nextAt = BigInt(next);
	}

	function withRuns(sc: ScheduledCommand, limit: number): ScheduledCommand {
		sc.runs = ctrl.store.listScheduleRuns(sc.name, limit);
		return sc;
	}

	function allSchedules(project: string): ScheduledCommand[] {
		const all = ctrl.store.listSchedules(project);
		if (all.length === 0) return all;
		const runsMap = ctrl.store.recentScheduleRuns(project, RUNS_LISTED);
		for (const sc of all) {
			sc.runs = runsMap.get(sc.name) ?? [];
		}
		return all;
	}

	function runSession(baseName: string, agent: string, project: string): string {
		for (let n = 1; n <= 20; n++) {
			const id = n === 1 ? baseName : `${baseName}-${n}`;
			try {
				ctrl.store.startSession(id, agent, project, "", ctrl.now());
				return id;
			} catch (err) {
				if (codeOf(err) !== ErrorCode.Conflict) throw err;
			}
		}
		throw hotbedError(ErrorCode.Conflict, `no free session name for ${baseName}`);
	}

	function cancelStartingRuns(schedule: string, scheduledOnly: boolean, why: string): void {
		for (const sr of ctrl.startingRuns.values()) {
			if (schedule && sr.schedule !== schedule) continue;
			if (scheduledOnly && !sr.scheduled) continue;
			if (!sr.stopWhy) sr.stopWhy = why;
			sr.abort.abort();
		}
	}

	async function notStarted(run: ScheduledRun, why: unknown): Promise<ScheduledRun> {
		const msg = fromUnknownError(why).message;
		const ended = BigInt(ctrl.now());
		run.outcome = RunOutcome.NOT_STARTED;
		run.reason = msg;
		run.endedAt = ended;
		const ok = ctrl.store.updateLiveScheduleRun(run);
		if (!ok) return ctrl.store.getScheduleRun(run.id);
		if (run.sessionId) {
			try {
				await ctrl.endSession(run.sessionId, "its scheduled run could not start");
			} catch (err) {
				ctrl.store.logDaemon(
					DaemonLogLevel.ERROR,
					"end session of run that could not start",
					{ session: run.sessionId, err: String(err) },
					ctrl.now(),
				);
			}
		}
		return run;
	}

	function reserveRun(
		sc: ScheduledCommand,
		slot: bigint | undefined,
		timeZone: string,
	): { run: ScheduledRun; key?: string; abort?: AbortController } {
		const nowMs = ctrl.now();
		const atMs = slot !== undefined ? Number(slot) : nowMs;
		const run = create(ScheduledRunSchema, {
			schedule: sc.name,
			slotAt: slot,
			startedAt: BigInt(nowMs),
			outcome: RunOutcome.RUNNING,
		});
		let project;
		try {
			project = ctrl.store.getProject(sc.project);
		} catch (err) {
			const he = fromUnknownError(err);
			run.outcome = RunOutcome.NOT_STARTED;
			run.reason = he.message;
			run.endedAt = BigInt(nowMs);
			return { run: ctrl.store.addScheduleRun(run) };
		}
		const sessName = `${sc.name}-${formatInTimeZone(atMs, timeZone, "session")}`;
		run.sessionId = runSession(sessName, `schedule:${sc.name}`, project.name);
		ctrl.store.addScheduleRun(run);
		const key = String(run.id);
		const abort = new AbortController();
		ctrl.startingRuns.set(key, {
			schedule: sc.name,
			scheduled: slot !== undefined,
			stopWhy: "",
			abort,
		});
		return { run, key, abort };
	}

	async function finishStart(
		sc: ScheduledCommand,
		run: ScheduledRun,
		key: string,
		abort: AbortController,
	): Promise<ScheduledRun> {
		const sr = ctrl.startingRuns.get(key);
		try {
			if (sr?.stopWhy) return await notStarted(run, new Error(sr.stopWhy));
			const w = await ws.createWhenFree(
				create(CreateWorkspaceRequestSchema, {
					sessionId: run.sessionId,
					repo: sc.repo,
					waitForCapacity: true,
					placement: create(PlacementSchema, {
						arch: sc.arch,
						bench: sc.command?.bench ?? false,
						memoryLowBytes: sc.command?.limits?.memoryLowBytes ?? 0n,
						memoryEstimateBytes: sc.command?.estimate?.memoryBytes ?? 0n,
						// Only a VM is sized.
						runtime: sc.command?.vmCpus || sc.command?.vmMemoryBytes ? "vm" : "",
						vmCpus: sc.command?.vmCpus ?? 0,
						vmMemoryBytes: sc.command?.vmMemoryBytes ?? 0n,
					}),
				}),
				abort.signal,
			);
			run.workspaceId = w.id;
			run.nodeId = w.nodeId;
			if (sr?.stopWhy) return await notStarted(run, new Error(sr.stopWhy));

			const cmdReq = clone(RunCommandRequestSchema, sc.command!);
			cmdReq.workspaceId = w.id;
			cmdReq.id = "";
			cmdReq.attached = false;
			cmdReq.port = 0;
			cmdReq.repo = sc.repo;
			cmdReq.placement = w.placement;
			const cmd = await ws.runCommandInternal(cmdReq);

			if (sr?.stopWhy) {
				try {
					await ws.stopCommandInternal(
						create(StopCommandRequestSchema, { id: cmd.id, internal: true, reason: sr.stopWhy }),
					);
				} catch {
					// best effort
				}
				return await notStarted(run, new Error(sr.stopWhy));
			}

			run.commandId = cmd.id;
			const ok = ctrl.store.updateLiveScheduleRun(run);
			if (!ok) {
				try {
					await ws.stopCommandInternal(
						create(StopCommandRequestSchema, {
							id: cmd.id,
							internal: true,
							reason: "scheduled run ended concurrently",
						}),
					);
				} catch {
					// best effort
				}
				return ctrl.store.getScheduleRun(run.id);
			}
			return run;
		} catch (err) {
			return await notStarted(run, sr?.stopWhy ? new Error(sr.stopWhy) : err);
		} finally {
			ctrl.startingRuns.delete(key);
		}
	}

	async function tickSchedules(): Promise<void> {
		// 1. Fail interrupted starts whose DO was evicted mid-launch.
		for (const run of ctrl.store.listAllLiveScheduleRuns()) {
			if (!run.commandId && !ctrl.startingRuns.has(String(run.id))) {
				await notStarted(run, new Error("the controller stopped while starting it"));
			}
		}
		// 2. Settle finished runs.
		await ctrl.settleRuns();

		// 3. Check due schedules.
		const nowMs = ctrl.now();
		const toLaunch: Array<{
			sc: ScheduledCommand;
			run: ScheduledRun;
			key: string;
			abort: AbortController;
		}> = [];
		for (const sc of ctrl.store.listSchedules("")) {
			if (sc.paused || sc.nextAt === undefined) continue;
			const first = Number(sc.nextAt);
			if (nowMs < first) continue;
			let parsed;
			try {
				parsed = parseCron(sc.cron, nowMs);
			} catch {
				continue;
			}
			const slots: number[] = [first];
			let t = parsed.next(first);
			while (t !== null && t <= nowMs) {
				slots.push(t);
				t = parsed.next(t);
			}
			const latest = slots[slots.length - 1];
			let missed = slots.slice(0, -1);
			if (nowMs - latest > SLOT_GRACE_MS) missed = slots;

			const next = parsed.next(nowMs);
			ctrl.store.setScheduleNextAt(sc.name, next !== null ? BigInt(next) : undefined);

			if (missed.length > 0) {
				const ended = BigInt(nowMs);
				const plural = missed.length === 1 ? "slot" : "slots";
				ctrl.store.addScheduleRun(
					create(ScheduledRunSchema, {
						schedule: sc.name,
						slotAt: BigInt(missed[missed.length - 1]),
						startedAt: ended,
						endedAt: ended,
						outcome: RunOutcome.MISSED,
						reason: `the controller was down: missed ${missed.length} ${plural} since ${formatInTimeZone(missed[0], parsed.timeZone, "hm")}`,
					}),
				);
			}
			if (missed.length < slots.length) {
				const prev = ctrl.store.liveScheduleRun(sc.name);
				if (prev) {
					const how =
						prev.slotAt !== undefined
							? `due at ${formatInTimeZone(Number(prev.slotAt), parsed.timeZone, "hm")}`
							: `started at ${formatInTimeZone(Number(prev.startedAt), parsed.timeZone, "hm")}`;
					const nowBig = BigInt(nowMs);
					ctrl.store.addScheduleRun(
						create(ScheduledRunSchema, {
							schedule: sc.name,
							slotAt: BigInt(latest),
							startedAt: nowBig,
							endedAt: nowBig,
							outcome: RunOutcome.SKIPPED,
							reason: `the run ${how} (${prev.sessionId}) was still in progress`,
						}),
					);
				} else {
					const res = reserveRun(sc, BigInt(latest), parsed.timeZone);
					if (res.key && res.abort) {
						toLaunch.push({ sc, run: res.run, key: res.key, abort: res.abort });
					}
				}
			}
		}

		await Promise.all(toLaunch.map((p) => finishStart(p.sc, p.run, p.key, p.abort)));
	}

	const impl: ServiceImpl<typeof ScheduleService> = {
		putSchedule(req, ctx) {
			if (!req.schedule) {
				throw hotbedError(ErrorCode.InvalidArgs, "no scheduled command");
			}
			const sc = clone(ScheduledCommandSchema, req.schedule);
			if (sc.node) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`placement by --node (${sc.node}) is no longer supported; specify requirements instead (--arch, --bench, --mem-low, --vm-cpus, --vm-mem)`,
				).withSuggestion(
					"drop --node, pass requirements such as --arch amd64|arm64, and run `hotbed upgrade`",
				);
			}
			if (!SCHEDULE_NAME_RE.test(sc.name) || sc.name.length > MAX_SCHEDULE_NAME) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`scheduled command name "${sc.name}" must be lowercase words joined by dashes, at most ${MAX_SCHEDULE_NAME} characters`,
				);
			}
			parseCron(sc.cron, ctrl.now());
			const repo = ctrl.repoById(sc.repo);
			const { project, changed } = ctrl.sessionProject(
				getCaller(ctx.values),
				sc.project,
				canonicalRemote(repo.url),
			);
			sc.project = project.name;
			if (!sc.command || sc.command.args.length === 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`scheduled command ${sc.name} has no command`,
				).withSuggestion("hotbed schedule add NAME REPO --cron '0 2 * * *' -- CMD...");
			}
			for (const kv of sc.command.env) {
				const eq = kv.indexOf("=");
				if (eq <= 0) {
					throw hotbedError(ErrorCode.InvalidArgs, `env "${kv}" is not KEY=VALUE`);
				}
			}
			sc.command.workspaceId = "";
			sc.command.id = "";
			sc.command.attached = false;
			sc.command.port = 0;

			const nowBig = BigInt(ctrl.now());
			sc.createdAt = nowBig;
			sc.updatedAt = nowBig;
			sc.runs = [];
			sc.reminder = undefined;
			try {
				const old = ctrl.store.getSchedule(sc.name);
				sc.createdAt = old.createdAt;
			} catch {
				// new schedule
			}
			planNext(sc);
			ctrl.store.putSchedule(sc);
			if (sc.paused) {
				cancelStartingRuns(sc.name, true, `schedule ${sc.name} was paused while starting`);
			}
			const out = withRuns(sc, RUNS_LISTED);
			if (changed) out.reminder = setupStepsReminder(project);
			return out;
		},

		listSchedules(req) {
			const project = ctrl.listedProject(req);
			return create(ListSchedulesResponseSchema, { schedules: allSchedules(project) });
		},

		getSchedule(req) {
			return withRuns(ctrl.store.getSchedule(req.name), RUNS_SHOWN);
		},

		deleteSchedule(req) {
			if (!ctrl.store.deleteSchedule(req.name)) {
				throw hotbedError(ErrorCode.NotFound, `no scheduled command ${req.name}`).withSuggestion(
					"hotbed schedule ls",
				);
			}
			cancelStartingRuns(req.name, false, `schedule ${req.name} was deleted while starting`);
			return create(DeleteScheduleResponseSchema);
		},

		async runSchedule(req) {
			const sc = ctrl.store.getSchedule(req.name);
			await ctrl.settleRuns();
			const prev = ctrl.store.liveScheduleRun(sc.name);
			if (prev) {
				throw hotbedError(
					ErrorCode.Conflict,
					`a run of ${sc.name} is in progress, in session ${prev.sessionId}`,
				).withSuggestion(
					`hotbed poll -s ${prev.sessionId} until it ends, or hotbed session end -s ${prev.sessionId} to stop it`,
				);
			}
			const parsed = parseCron(sc.cron, ctrl.now());
			const res = reserveRun(sc, undefined, parsed.timeZone);
			if (!res.key || !res.abort) return res.run;
			return finishStart(sc, res.run, res.key, res.abort);
		},

		pauseSchedule(req) {
			const sc = ctrl.store.getSchedule(req.name);
			sc.paused = req.paused;
			sc.updatedAt = BigInt(ctrl.now());
			planNext(sc);
			ctrl.store.putSchedule(sc);
			if (sc.paused) {
				cancelStartingRuns(sc.name, true, `schedule ${sc.name} was paused while starting`);
			}
			return withRuns(sc, RUNS_LISTED);
		},
	};

	return {
		impl,
		runner: { tickSchedules, allSchedules },
	};
}
