import { clone, create } from "@bufbuild/protobuf";
import {
	AppliedStepSchema,
	BaseFreshenSchema,
	BaseRepoSchema,
	BaseState,
	CommandEventSchema,
	CommandState,
	DurationStatsSchema,
	FreshenPhaseSchema,
	FreshenState,
	FreshenSummarySchema,
	NodeProjectFreshenSummarySchema,
	StepTimingSummarySchema,
	WorkspaceState,
	type AppliedStep,
	type Base,
	type BaseUsage,
	type Command,
	type CommandEvent,
	type CommandUsage,
	type BaseFreshen,
	type DurationStats,
	type FreshenSummary,
	type NodeProjectFreshenSummary,
	type StepTimingSummary,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import type { NodeView } from "./placement.ts";

export function isFreshenActive(s: FreshenState): boolean {
	return (
		s === FreshenState.QUEUED ||
		s === FreshenState.RUNNING ||
		s === FreshenState.PARKED ||
		s === FreshenState.PAUSED ||
		s === FreshenState.FINALIZING
	);
}

export function runtimeOfBase(b?: Base): string {
	if (!b) return "";
	if (b.runtime) return b.runtime;
	if (b.cladefsCell) {
		if (b.id.includes("-vm-")) return "vm";
		if (b.id.includes("-fuse-")) return "cladefs";
		return "cladefs";
	}
	return "";
}

function failedStepOf(steps: AppliedStep[]): string {
	for (const s of steps) {
		if (s.ran && !s.ok) return s.name;
	}
	return "";
}

function sumRanStepsSeconds(steps: AppliedStep[]): number {
	let sum = 0;
	for (const s of steps) {
		if (s.ran && s.seconds > 0) sum += s.seconds;
	}
	return sum;
}

export function computeParkedStats(
	timeline: CommandEvent[],
	endOrNowMs: number,
	state: FreshenState,
): { parks: number; parkedSeconds: number } {
	let parks = 0;
	let parkedMs = 0;
	let parkedAt: number | null = null;
	for (const ev of timeline) {
		const at = Number(ev.at);
		if (ev.what === "parked") {
			parks++;
			if (parkedAt === null) parkedAt = at;
		} else if (
			parkedAt !== null &&
			(ev.what === "thawed" ||
				ev.what === "exited" ||
				ev.what === "cancelled" ||
				ev.what === "paused")
		) {
			if (at > parkedAt) parkedMs += at - parkedAt;
			parkedAt = null;
		}
	}
	if (parkedAt !== null && state === FreshenState.PARKED && endOrNowMs > parkedAt) {
		parkedMs += endOrNowMs - parkedAt;
	}
	return { parks, parkedSeconds: parkedMs / 1000 };
}

function isTerminalState(s: FreshenState): boolean {
	return (
		s === FreshenState.SUCCEEDED ||
		s === FreshenState.FAILED ||
		s === FreshenState.CANCELLED ||
		s === FreshenState.DROPPED
	);
}

function stateFromCommand(c: Command): FreshenState {
	switch (c.state) {
		case CommandState.QUEUED:
			return FreshenState.QUEUED;
		case CommandState.RUNNING:
			return FreshenState.RUNNING;
		case CommandState.PARKED:
			return FreshenState.PARKED;
		case CommandState.PAUSED:
			return FreshenState.PAUSED;
		case CommandState.SUCCEEDED:
			return FreshenState.FINALIZING;
		case CommandState.FAILED:
			return FreshenState.FAILED;
		case CommandState.CANCELLED:
			return FreshenState.CANCELLED;
		default:
			return FreshenState.UNSPECIFIED;
	}
}

function parseProjectBaseFromArgv(argv: string): {
	project: string;
	baseId: string;
	generation: number;
} {
	const mGen = /^freshen\s+(\S+)\s+g(\d+)$/.exec(argv.trim());
	if (mGen) {
		const gen = Number(mGen[2]);
		return { project: mGen[1], baseId: `${mGen[1]}-g${gen}`, generation: gen };
	}
	const mId = /^freshen\s+(\S+)$/.exec(argv.trim());
	if (mId) {
		const id = mId[1];
		const dash = id.indexOf("-");
		return { project: dash > 0 ? id.slice(0, dash) : id, baseId: id, generation: 0 };
	}
	return { project: "", baseId: "", generation: 0 };
}

export function freshenIdForBase(nodeId: string, b: Base): string {
	return b.command || `base:${nodeId}:${b.id}`;
}

export function mergeBaseIntoFreshen(
	existing: BaseFreshen | null,
	nodeId: string,
	b: Base,
	what: string,
	detail: string,
	eventAtMs: number,
): BaseFreshen {
	const f = existing
		? clone(BaseFreshenSchema, existing)
		: create(BaseFreshenSchema, {
				id: freshenIdForBase(nodeId, b),
				baseId: b.id,
				nodeId,
				project: b.project,
				generation: b.generation,
				runtime: runtimeOfBase(b),
				createdAt: b.createdAt > 0n ? b.createdAt : BigInt(eventAtMs),
			});

	f.baseId = b.id || f.baseId;
	f.nodeId = nodeId || f.nodeId;
	f.project = b.project || f.project;
	if (b.generation > 0) f.generation = b.generation;
	f.runtime = runtimeOfBase(b);
	f.cold = b.cold;
	if (b.coldReason) f.coldReason = b.coldReason;
	if (b.fromBaseId) f.fromBaseId = b.fromBaseId;
	if (b.createdAt > 0n && (f.createdAt === 0n || b.createdAt < f.createdAt)) {
		f.createdAt = b.createdAt;
	}
	if (b.prepSeconds > 0) f.prepSeconds = b.prepSeconds;
	if (b.gitSeconds > 0) f.gitSeconds = b.gitSeconds;
	if (b.finalizeSeconds > 0) f.finalizeSeconds = b.finalizeSeconds;
	if (b.peakMemoryBytes > f.peakMemoryBytes) f.peakMemoryBytes = b.peakMemoryBytes;
	if (b.units > 0) f.units = b.units;
	if (b.sizeBytes > 0n) f.sizeBytes = b.sizeBytes;
	if (b.recipe) f.recipe = b.recipe;
	if (b.progress > 0 || b.state === BaseState.WARM) f.progress = b.progress;
	if (b.repos.length > 0) {
		f.repos = b.repos.map((r) => clone(BaseRepoSchema, r));
	}
	if (b.steps.length > 0) {
		f.steps = b.steps.map((s) => clone(AppliedStepSchema, s));
		f.stepsSeconds = sumRanStepsSeconds(f.steps);
	}
	if (b.phases.length > 0) {
		f.phases = b.phases.map((p) => clone(FreshenPhaseSchema, p));
	}
	if (b.totalSteps > 0) f.totalSteps = b.totalSteps;
	f.currentStep = b.currentStep;
	f.currentStepIndex = b.currentStepIndex;

	if (what === "building") {
		if (f.state === FreshenState.UNSPECIFIED) {
			f.state = FreshenState.QUEUED;
		}
		if (detail.startsWith("from g") && !f.fromBaseId) {
			f.fromBaseId = `${b.project}-${detail.slice("from ".length)}`;
		} else if (detail.startsWith("from ") && detail !== "from nothing" && !f.fromBaseId) {
			f.fromBaseId = detail.slice("from ".length);
		}
	} else if (what === "progress") {
		if (!isTerminalState(f.state) && f.state !== FreshenState.FINALIZING) {
			f.state = FreshenState.RUNNING;
		}
		f.currentPhase = b.currentStep ? `step:${b.currentStep}` : "steps";
	} else if (what === "finalizing") {
		if (!isTerminalState(f.state)) {
			f.state = FreshenState.FINALIZING;
		}
		f.currentPhase = detail || "finalizing";
		f.currentStep = "";
		f.currentStepIndex = 0;
	} else if (what === "dropped") {
		f.state = FreshenState.DROPPED;
		f.endedAt = f.endedAt ?? BigInt(eventAtMs);
		f.reason = b.failureReason || detail || "superseded, and no workspace waits for it";
		f.currentPhase = "";
		f.currentStep = "";
		f.currentStepIndex = 0;
	} else if (what === "failed" || b.state === BaseState.FAILED) {
		if (f.state !== FreshenState.DROPPED) {
			f.state = FreshenState.FAILED;
		}
		f.endedAt = b.builtAt ?? f.endedAt ?? BigInt(eventAtMs);
		f.failedStep = failedStepOf(f.steps);
		f.reason = b.failureReason || detail || f.reason;
		if (b.buildSeconds > 0 && f.runSeconds === 0) f.runSeconds = b.buildSeconds;
		f.currentPhase = "";
		f.currentStep = "";
		f.currentStepIndex = 0;
	} else if (what === "warm" || b.state === BaseState.WARM) {
		f.state = FreshenState.SUCCEEDED;
		f.progress = 1;
		f.endedAt = b.builtAt ?? f.endedAt ?? BigInt(eventAtMs);
		if (b.buildSeconds > 0) f.runSeconds = b.buildSeconds;
		f.currentPhase = "";
		f.currentStep = "";
		f.currentStepIndex = 0;
		f.reason = "";
	} else if (what === "stale" && !isTerminalState(f.state)) {
		if (b.builtAt !== undefined && b.builtAt > 0n) {
			f.state = FreshenState.SUCCEEDED;
			f.progress = 1;
			f.endedAt = b.builtAt;
			if (b.buildSeconds > 0) f.runSeconds = b.buildSeconds;
		} else {
			f.state = FreshenState.DROPPED;
			f.endedAt = BigInt(eventAtMs);
			f.reason = b.failureReason || detail || "superseded";
		}
		f.currentPhase = "";
		f.currentStep = "";
		f.currentStepIndex = 0;
	}

	recomputeDurations(f, Number(f.endedAt ?? BigInt(eventAtMs)));
	return f;
}

export function mergeCommandIntoFreshen(
	existing: BaseFreshen | null,
	nodeId: string,
	c: Command,
	what: string,
	detail: string,
	eventAtMs: number,
): BaseFreshen {
	const parsed = parseProjectBaseFromArgv(c.argv);
	const f = existing
		? clone(BaseFreshenSchema, existing)
		: create(BaseFreshenSchema, {
				id: c.id,
				baseId: parsed.baseId,
				nodeId,
				project: c.project || parsed.project,
				generation: parsed.generation,
				runtime:
					c.runtime ||
					(parsed.baseId.includes("-vm-")
						? "vm"
						: parsed.baseId.includes("-fuse-")
							? "cladefs"
							: ""),
				createdAt: c.queuedAt > 0n ? c.queuedAt : BigInt(eventAtMs),
			});

	if (!f.project && (c.project || parsed.project)) f.project = c.project || parsed.project;
	if (!f.baseId && parsed.baseId) f.baseId = parsed.baseId;
	if (f.generation === 0 && parsed.generation > 0) f.generation = parsed.generation;
	if (f.createdAt === 0n && c.queuedAt > 0n) f.createdAt = c.queuedAt;
	if (c.startedAt !== undefined) f.startedAt = c.startedAt;
	if (c.limits && c.limits.memoryLowBytes > 0n) {
		f.memoryLowBytes = c.limits.memoryLowBytes;
	}
	if (c.exitCode !== undefined) f.exitCode = c.exitCode;
	if (c.timeline.length > 0) {
		f.timeline = c.timeline.map((ev) => clone(CommandEventSchema, ev));
	}

	const cmdState = stateFromCommand(c);
	if (!isTerminalState(f.state)) {
		if (cmdState === FreshenState.CANCELLED) {
			f.state = detail.includes("superseded") ? FreshenState.DROPPED : FreshenState.CANCELLED;
			f.endedAt = c.endedAt ?? BigInt(eventAtMs);
			f.reason = c.reason || detail || f.reason;
		} else if (cmdState === FreshenState.FAILED) {
			f.state = FreshenState.FAILED;
			f.endedAt = c.endedAt ?? BigInt(eventAtMs);
			f.reason = c.reason || detail || f.reason;
		} else if (cmdState === FreshenState.FINALIZING) {
			f.state = FreshenState.FINALIZING;
			if (!f.currentPhase) f.currentPhase = "finalizing";
		} else if (cmdState !== FreshenState.UNSPECIFIED) {
			f.state = cmdState;
			if (
				cmdState === FreshenState.QUEUED ||
				cmdState === FreshenState.PARKED ||
				cmdState === FreshenState.PAUSED
			) {
				if (c.reason) f.reason = c.reason;
				f.currentPhase = cmdState === FreshenState.QUEUED ? "queue" : "parked";
			} else if (cmdState === FreshenState.RUNNING) {
				f.reason = "";
				if (!f.currentPhase || f.currentPhase === "queue" || f.currentPhase === "parked") {
					f.currentPhase = f.currentStep ? `step:${f.currentStep}` : "git";
				}
			}
		}
	} else if (c.reason && !f.reason) {
		f.reason = c.reason;
	}

	if (c.startedAt !== undefined && c.queuedAt > 0n && c.startedAt >= c.queuedAt) {
		f.queueSeconds = Number(c.startedAt - c.queuedAt) / 1000;
	}
	if (c.startedAt !== undefined && c.endedAt !== undefined && c.endedAt >= c.startedAt) {
		f.runSeconds = Number(c.endedAt - c.startedAt) / 1000;
	}

	recomputeDurations(f, Number(f.endedAt ?? BigInt(eventAtMs)));
	return f;
}

function recomputeDurations(f: BaseFreshen, endOrNowMs: number): void {
	const createdMs = Number(f.createdAt);
	const startedMs = f.startedAt !== undefined ? Number(f.startedAt) : null;
	const endedMs = f.endedAt !== undefined ? Number(f.endedAt) : null;
	const refEndMs = endedMs ?? endOrNowMs;

	if (createdMs > 0 && refEndMs >= createdMs) {
		f.totalSeconds = (refEndMs - createdMs) / 1000;
	} else if (f.runSeconds > 0) {
		f.totalSeconds = f.prepSeconds + f.queueSeconds + f.runSeconds + f.finalizeSeconds;
	}

	if (f.state === FreshenState.QUEUED && createdMs > 0 && refEndMs >= createdMs) {
		const queuedAtMs = f.timeline.find((e) => e.what === "queued")?.at;
		const qStart = queuedAtMs !== undefined && queuedAtMs > 0n ? Number(queuedAtMs) : createdMs;
		f.queueSeconds = Math.max(0, (refEndMs - qStart) / 1000);
	} else if (startedMs !== null && f.queueSeconds === 0) {
		const queuedAtMs = f.timeline.find((e) => e.what === "queued")?.at;
		const qStart = queuedAtMs !== undefined && queuedAtMs > 0n ? Number(queuedAtMs) : createdMs;
		if (qStart > 0 && startedMs >= qStart) {
			f.queueSeconds = (startedMs - qStart) / 1000;
		}
	}

	if (
		startedMs !== null &&
		(f.state === FreshenState.RUNNING ||
			f.state === FreshenState.PARKED ||
			f.state === FreshenState.PAUSED) &&
		refEndMs >= startedMs
	) {
		f.runSeconds = (refEndMs - startedMs) / 1000;
	}

	if (f.steps.length > 0) {
		f.stepsSeconds = sumRanStepsSeconds(f.steps);
	}

	const parked = computeParkedStats(f.timeline, refEndMs, f.state);
	f.parks = Math.max(f.parks, parked.parks);
	f.parkedSeconds = Math.max(f.parkedSeconds, parked.parkedSeconds);

	if (f.failedStep === "" && f.state === FreshenState.FAILED) {
		f.failedStep = failedStepOf(f.steps);
	}
}

export function enrichFreshenLive(
	input: BaseFreshen,
	nodeHost: string,
	nowMs: number,
	base?: Base,
	cmd?: Command,
	hbUsage?: CommandUsage,
	hbBase?: BaseUsage,
	waitingWorkspaces?: string[],
): BaseFreshen {
	let f = clone(BaseFreshenSchema, input);
	if (nodeHost) f.node = nodeHost;
	if (base && isFreshenActive(f.state)) {
		if (base.state === BaseState.WARM) {
			f = mergeBaseIntoFreshen(f, f.nodeId, base, "warm", "", nowMs);
		} else if (base.state === BaseState.FAILED) {
			f = mergeBaseIntoFreshen(f, f.nodeId, base, "failed", base.failureReason, nowMs);
		} else {
			if (base.steps.length > 0) f.steps = base.steps.map((s) => clone(AppliedStepSchema, s));
			if (base.phases.length > 0) f.phases = base.phases.map((p) => clone(FreshenPhaseSchema, p));
			if (base.currentStep) f.currentStep = base.currentStep;
			if (base.currentStepIndex > 0) f.currentStepIndex = base.currentStepIndex;
			if (base.totalSteps > 0) f.totalSteps = base.totalSteps;
			if (base.gitSeconds > 0) f.gitSeconds = base.gitSeconds;
			if (base.prepSeconds > 0) f.prepSeconds = base.prepSeconds;
			if (base.coldReason) f.coldReason = base.coldReason;
			if (base.fromBaseId) f.fromBaseId = base.fromBaseId;
			if (base.currentStepStartedAt > 0n && nowMs > Number(base.currentStepStartedAt)) {
				f.currentStepSeconds = (nowMs - Number(base.currentStepStartedAt)) / 1000;
			}
		}
	}
	if (cmd && isFreshenActive(f.state)) {
		f = mergeCommandIntoFreshen(f, f.nodeId, cmd, "", "", nowMs);
	}
	if (hbUsage && f.state === FreshenState.QUEUED && hbUsage.reason) {
		f.reason = hbUsage.reason;
	}
	if (hbBase && isFreshenActive(f.state) && hbBase.progress > f.progress) {
		f.progress = hbBase.progress;
	}
	if (waitingWorkspaces && waitingWorkspaces.length > 0) {
		f.waitingWorkspaces = [...waitingWorkspaces].sort();
	}
	if (isFreshenActive(f.state)) {
		recomputeDurations(f, nowMs);
	}
	return f;
}

/**
 * Stored freshens as they stand now: each enriched with its node's live base,
 * command, heartbeat and waiting workspaces, and an active one whose base and
 * command have both left the node marked failed.
 */
export function withLiveState(
	stored: BaseFreshen[],
	views: NodeView[],
	nowMs: number,
): BaseFreshen[] {
	const hostById = new Map<string, string>();
	const cmdById = new Map<string, Command>();
	const baseByKey = new Map<string, Base>();
	const hbCmdById = new Map<string, CommandUsage>();
	const hbBaseByKey = new Map<string, BaseUsage>();
	const waitingByKey = new Map<string, string[]>();

	for (const nv of views) {
		hostById.set(nv.node.id, nv.node.hostname);
		for (const c of nv.commands) {
			if (c.kind === "freshen") cmdById.set(c.id, c);
		}
		for (const b of nv.bases) baseByKey.set(`${nv.node.id}:${b.id}`, b);
		if (nv.online && nv.hb) {
			for (const cu of nv.hb.commands) hbCmdById.set(cu.id, cu);
			for (const bu of nv.hb.bases) hbBaseByKey.set(`${nv.node.id}:${bu.id}`, bu);
		}
		for (const w of nv.workspaces) {
			if (w.state === WorkspaceState.PREPARING && w.baseId) {
				const k = `${nv.node.id}:${w.baseId}`;
				let list = waitingByKey.get(k);
				if (!list) {
					list = [];
					waitingByKey.set(k, list);
				}
				list.push(w.id);
			}
		}
	}

	return stored.map((input) => {
		const f = clone(BaseFreshenSchema, input);
		const bKey = `${f.nodeId}:${f.baseId}`;
		const liveBase = baseByKey.get(bKey);
		const liveCmd = cmdById.get(f.id);
		if (isFreshenActive(f.state) && !liveBase && !liveCmd) {
			// Neither the base nor the freshen command exists on the worker anymore.
			f.state = FreshenState.FAILED;
			if (!f.reason) f.reason = "ended before telemetry recorded (base no longer on node)";
			if (f.endedAt === undefined) f.endedAt = f.createdAt;
		}
		return enrichFreshenLive(
			f,
			hostById.get(f.nodeId) ?? f.node,
			nowMs,
			liveBase,
			liveCmd,
			hbCmdById.get(f.id),
			hbBaseByKey.get(bKey),
			waitingByKey.get(bKey),
		);
	});
}

/** The p-quantile of xs (nearest rank), 0 for none. */
export function quantile(xs: number[], p: number): number {
	if (xs.length === 0) return 0;
	const sorted = [...xs].sort((a, b) => a - b);
	return sorted[Math.round(p * (sorted.length - 1))];
}

function durationStats(values: number[]): DurationStats {
	const pos = values.filter((v) => v > 0);
	if (pos.length === 0) return create(DurationStatsSchema);
	let sum = 0;
	let max = 0;
	for (const v of pos) {
		sum += v;
		if (v > max) max = v;
	}
	return create(DurationStatsSchema, {
		count: pos.length,
		totalSeconds: sum,
		p50Seconds: quantile(pos, 0.5),
		p95Seconds: quantile(pos, 0.95),
		maxSeconds: max,
	});
}

export function buildFreshenSummary(
	freshens: BaseFreshen[],
	windowSinceMs: number,
): FreshenSummary {
	const sum = create(FreshenSummarySchema, {
		total: freshens.length,
		windowSince: BigInt(windowSinceMs),
	});

	const totalDurs: number[] = [];
	const queueDurs: number[] = [];
	const prepDurs: number[] = [];
	const gitDurs: number[] = [];
	const stepsDurs: number[] = [];
	const parkedDurs: number[] = [];
	const finalizeDurs: number[] = [];

	const stepAcc = new Map<
		string,
		{
			project: string;
			step: string;
			runs: number;
			failures: number;
			durs: number[];
			total: number;
			max: number;
		}
	>();
	const npAcc = new Map<
		string,
		{
			node: string;
			project: string;
			runtime: string;
			items: BaseFreshen[];
		}
	>();

	for (const f of freshens) {
		switch (f.state) {
			case FreshenState.QUEUED:
				sum.active++;
				sum.queued++;
				break;
			case FreshenState.RUNNING:
				sum.active++;
				sum.running++;
				break;
			case FreshenState.PARKED:
			case FreshenState.PAUSED:
				sum.active++;
				sum.parked++;
				break;
			case FreshenState.FINALIZING:
				sum.active++;
				sum.finalizing++;
				break;
			case FreshenState.SUCCEEDED:
				sum.succeeded++;
				break;
			case FreshenState.FAILED:
				sum.failed++;
				break;
			case FreshenState.CANCELLED:
			case FreshenState.DROPPED:
				sum.cancelled++;
				break;
		}
		if (f.cold) sum.cold++;
		else sum.incremental++;
		sum.waitingWorkspaces += f.waitingWorkspaces.length;
		sum.totalParks += f.parks;

		if (f.totalSeconds > 0) totalDurs.push(f.totalSeconds);
		if (f.queueSeconds > 0) queueDurs.push(f.queueSeconds);
		if (f.prepSeconds > 0) prepDurs.push(f.prepSeconds);
		if (f.gitSeconds > 0) gitDurs.push(f.gitSeconds);
		if (f.stepsSeconds > 0) stepsDurs.push(f.stepsSeconds);
		if (f.parkedSeconds > 0) parkedDurs.push(f.parkedSeconds);
		if (f.finalizeSeconds > 0) finalizeDurs.push(f.finalizeSeconds);

		for (const s of f.steps) {
			if (!s.ran) continue;
			const isFailedStep = f.state === FreshenState.FAILED && f.failedStep === s.name;
			if (!s.ok && !isFailedStep) continue;
			const key = `${f.project}\0${s.name}`;
			let acc = stepAcc.get(key);
			if (!acc) {
				acc = {
					project: f.project,
					step: s.name,
					runs: 0,
					failures: 0,
					durs: [],
					total: 0,
					max: 0,
				};
				stepAcc.set(key, acc);
			}
			acc.runs++;
			if (isFailedStep) acc.failures++;
			if (s.seconds > 0) {
				acc.durs.push(s.seconds);
				acc.total += s.seconds;
				if (s.seconds > acc.max) acc.max = s.seconds;
			}
		}

		const npKey = `${f.node || f.nodeId}\0${f.project}\0${f.runtime || ""}`;
		let np = npAcc.get(npKey);
		if (!np) {
			np = {
				node: f.node || f.nodeId,
				project: f.project,
				runtime: f.runtime || "",
				items: [],
			};
			npAcc.set(npKey, np);
		}
		np.items.push(f);
	}

	sum.totalDuration = durationStats(totalDurs);
	sum.queueDuration = durationStats(queueDurs);
	sum.prepDuration = durationStats(prepDurs);
	sum.gitDuration = durationStats(gitDurs);
	sum.stepsDuration = durationStats(stepsDurs);
	sum.parkedDuration = durationStats(parkedDurs);
	sum.finalizeDuration = durationStats(finalizeDurs);

	const stepSummaries: StepTimingSummary[] = [];
	for (const acc of stepAcc.values()) {
		stepSummaries.push(
			create(StepTimingSummarySchema, {
				project: acc.project,
				step: acc.step,
				runs: acc.runs,
				failures: acc.failures,
				totalSeconds: acc.total,
				p50Seconds: quantile(acc.durs, 0.5),
				maxSeconds: acc.max,
			}),
		);
	}
	stepSummaries.sort((a, b) => b.totalSeconds - a.totalSeconds || b.failures - a.failures);
	sum.slowestSteps = stepSummaries.slice(0, 12);

	const npSummaries: NodeProjectFreshenSummary[] = [];
	for (const np of npAcc.values()) {
		// Sort newest first by createdAt to compute consecutive failure / cold streaks.
		const sorted = [...np.items].sort((a, b) => Number(b.createdAt - a.createdAt));
		let coldRuns = 0;
		let succeeded = 0;
		let failed = 0;
		let cancelled = 0;
		let totalQueue = 0;
		let totalParked = 0;
		let totalFinalize = 0;
		let waitingWs = 0;
		const durs: number[] = [];
		const coldReasons: Record<string, number> = {};
		let active: BaseFreshen | undefined;

		for (const f of sorted) {
			if (f.cold) {
				coldRuns++;
				const cr = f.coldReason || "unknown";
				coldReasons[cr] = (coldReasons[cr] ?? 0) + 1;
			}
			if (f.state === FreshenState.SUCCEEDED) succeeded++;
			else if (f.state === FreshenState.FAILED) failed++;
			else if (f.state === FreshenState.CANCELLED || f.state === FreshenState.DROPPED) cancelled++;
			else if (isFreshenActive(f.state) && !active) active = f;

			if (f.totalSeconds > 0) durs.push(f.totalSeconds);
			totalQueue += f.queueSeconds;
			totalParked += f.parkedSeconds;
			totalFinalize += f.finalizeSeconds;
			waitingWs += f.waitingWorkspaces.length;
		}

		let consecFail = 0;
		for (const f of sorted) {
			if (isFreshenActive(f.state)) continue;
			if (
				f.state === FreshenState.FAILED ||
				f.state === FreshenState.DROPPED ||
				f.state === FreshenState.CANCELLED
			) {
				consecFail++;
			} else {
				break;
			}
		}
		let consecCold = 0;
		for (const f of sorted) {
			if (f.cold) consecCold++;
			else break;
		}

		let maxDur = 0;
		for (const d of durs) if (d > maxDur) maxDur = d;

		npSummaries.push(
			create(NodeProjectFreshenSummarySchema, {
				node: np.node,
				project: np.project,
				runtime: np.runtime,
				runs: sorted.length,
				coldRuns,
				succeeded,
				failed,
				cancelled,
				consecutiveFailures: consecFail,
				consecutiveCold: consecCold,
				p50TotalSeconds: quantile(durs, 0.5),
				maxTotalSeconds: maxDur,
				totalQueueSeconds: totalQueue,
				totalParkedSeconds: totalParked,
				totalFinalizeSeconds: totalFinalize,
				activeState: active?.state ?? FreshenState.UNSPECIFIED,
				activeId: active?.id ?? "",
				activeStep: active?.currentStep ?? active?.currentPhase ?? "",
				activeElapsedSeconds: active?.totalSeconds ?? 0,
				activeReason: active?.reason ?? "",
				waitingWorkspaces: waitingWs,
				coldReasons,
			}),
		);
	}
	npSummaries.sort((a, b) => {
		const aActive = a.activeState !== FreshenState.UNSPECIFIED ? 1 : 0;
		const bActive = b.activeState !== FreshenState.UNSPECIFIED ? 1 : 0;
		if (bActive !== aActive) return bActive - aActive;
		if (b.waitingWorkspaces !== a.waitingWorkspaces)
			return b.waitingWorkspaces - a.waitingWorkspaces;
		if (b.consecutiveFailures !== a.consecutiveFailures)
			return b.consecutiveFailures - a.consecutiveFailures;
		return b.maxTotalSeconds - a.maxTotalSeconds;
	});
	sum.byNodeProject = npSummaries;
	return sum;
}
