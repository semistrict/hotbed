import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create, toBinary } from "@bufbuild/protobuf";
import {
	BaseState,
	ClusterService,
	CommandDiagnosticsSchema,
	CommandState,
	DaemonLogLevel,
	DeleteRepoResponseSchema,
	DeleteWorkerConfigResponseSchema,
	FreshenState,
	GetCommandDiagnosticsResponseSchema,
	GetFreshenResponseSchema,
	GetWorkerConfigResponseSchema,
	HeartbeatResponseSchema,
	HourBucketSchema,
	JoinResponseSchema,
	ListBasesResponseSchema,
	ListEventsResponseSchema,
	ListFreshensResponseSchema,
	ListNodesResponseSchema,
	ListPlacementsResponseSchema,
	ListProjectsResponseSchema,
	ListReposResponseSchema,
	ListStepsResponseSchema,
	LoadSampleSchema,
	NodeBaseSchema,
	NodeDetailSchema,
	NodeHourSchema,
	NodeHoursSchema,
	NodeSystemUpgradeSchema,
	NodeUsageSchema,
	PushCommandOutputResponseSchema,
	PushDaemonLogsResponseSchema,
	PushEventsResponseSchema,
	RepoSchema,
	RepoSummarySchema,
	SampleSchema,
	SamplesSchema,
	SaveCommandDiagnosticsResponseSchema,
	SaveCommandSampleResponseSchema,
	ListCommandSamplesResponseSchema,
	SnapshotSchema,
	StepOnNodeSchema,
	StepSchema,
	StepState,
	StepStatusSchema,
	TotalsSchema,
	UpgradeSystemResponseSchema,
	WorkerConfigSchema,
	WorkerUpgradeSystemRequestSchema,
	type Base,
	type BaseFreshen,
	type ClusterEvent,
	type Command,
	type CommandDiagnostics,
	type FinishedCommand,
	type HeartbeatRequest,
	type HourBucket,
	type LoadSample,
	type NodeHours,
	type NodeSystemUpgrade,
	type PlacementDecision,
	type Repo,
	type RepoSummary,
	type Samples,
	type Totals,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { isSupportedPlatform } from "../builds.ts";
import { isOrgAdminCaller, sha256Hex } from "../auth.ts";
import { getCaller } from "../connect-adapter.ts";
import { formatOrgToken } from "../workos.ts";
import { ensureRepoProject, hostingFor, projectsWithCounts, repoById } from "../project-scope.ts";
import { PROJECT_NAME_RE, REPO_ID_RE, STEP_NAME_RE, type ControllerContext } from "../context.ts";
import { setupStepsReminder } from "../reminders.ts";
import { ErrorCode, codeOf, hotbedError } from "../errors.ts";
import { parseDurationMs } from "../durations.ts";
import { buildFreshenSummary, isFreshenActive, quantile, withLiveState } from "../freshens.ts";
import { canonicalRemote, validateRepoURL } from "../gitops.ts";
import { parseBuildNumber } from "../version.ts";
import { stepHash } from "../ids.ts";
import { isCommandLive } from "../wakeups.ts";
import { KEEP_EVENTS, KEEP_PLACEMENTS_MS, type ForkRow, type SampleRow } from "../store/index.ts";
import { applyOrgCladefsTenant, assertNoTenantOverride } from "../worker-config.ts";
import type { ScheduleRunner } from "./schedules.ts";

const LIVE_WINDOW_MS = 15 * 60_000;
const SAMPLE_SECONDS = 5;
const EVENTS_SHOWN = 80;
const PLACEMENTS_SHOWN = 300;
const ADMISSION_EVENTS_SHOWN = 500;
const ADMISSION_KINDS = ["hold", "unhold", "wait"];
const MAX_DIAGNOSTICS_RESPONSE_BYTES = 2 * 1024 * 1024;

const DEFAULT_FRESHEN_WINDOW_MS = 24 * 3600_000;
const DEFAULT_FRESHEN_LIMIT = 30;

// How long a project base nobody holds is kept, once a newer base replaced
// it: long enough for a node that was just handed it to fork it, and for a
// worker that heartbeats without its bases (upgrading, or its cladefs down) to
// hold them again.
const SHARED_BASE_RETIRE_GRACE_MS = 15 * 60_000;

// Whether a heartbeat says which bases its worker holds: one whose cladefs
// backend is up (it offers vm or cladefs) on the controller's build (an older
// one may not know held_bases). Others keep the holds they last reported.
export function reportsHeldBases(req: HeartbeatRequest, wantBuild: number): boolean {
	if (req.storageError) return false;
	if (!(req.resources?.runtimes ?? []).some((r) => r === "vm" || r === "cladefs")) return false;
	return wantBuild === 0 || parseBuildNumber(req.resources?.version ?? "") >= wantBuild;
}

function parseFreshenSinceMs(raw: string, nowMs: number): number {
	const s = raw.trim().toLowerCase();
	if (!s) return nowMs - DEFAULT_FRESHEN_WINDOW_MS;
	if (s === "all" || s === "0") return 0;
	return Math.max(0, nowMs - parseDurationMs(s, "--since duration", '"1h", "24h", "7d", or "all"'));
}

function checkProjectName(name: string): void {
	if (!PROJECT_NAME_RE.test(name)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`project name "${name}" must be lowercase letters, digits and dashes, at most 40`,
		);
	}
}

function canonicalRemotes(repos: string[]): string[] {
	return repos.map((r) => {
		const c = canonicalRemote(r);
		if (!c) throw hotbedError(ErrorCode.InvalidArgs, `repo "${r}" is not a git remote`);
		return c;
	});
}

function hourOf(ms: number): number {
	return Math.floor(ms / 3_600_000) * 3_600_000;
}

function dayHours(nowMs: number): number[] {
	const cur = hourOf(nowMs);
	const out: number[] = [];
	for (let i = 0; i < 24; i++) {
		out.push(cur - (23 - i) * 3_600_000);
	}
	return out;
}

function summarizeRepo(repo: Repo, bases: Base[]): RepoSummary {
	const cleaned = repo.url.replace(/\/+$/, "");
	const parts = cleaned.split(/[/:]/).filter(Boolean);
	const rawName =
		parts.length >= 2 ? `${parts[parts.length - 2]}/${parts[parts.length - 1]}` : cleaned;
	const out = create(RepoSummarySchema, {
		id: repo.id,
		name: rawName.replace(/\.git$/, ""),
		remote: canonicalRemote(repo.url),
	});
	let cold: Base | undefined;
	let checkout: Base["repos"][number] | undefined;
	for (const b of bases) {
		if (!b.cold || (b.state !== BaseState.WARM && b.state !== BaseState.STALE)) continue;
		const r = b.repos.find((x) => x.repo === repo.id);
		if (r && (!cold || b.generation > cold.generation)) {
			cold = b;
			checkout = r;
		}
	}
	if (!cold || !checkout) return out;
	out.units = cold.units;
	out.coldBuildSeconds = cold.buildSeconds;
	if (checkout.toolchain.startsWith("rust")) out.language = "rust";
	else if (checkout.toolchain.startsWith("go")) out.language = "go";
	else if (checkout.toolchain.startsWith("node")) out.language = "typescript";
	return out;
}

interface R2Totals {
	up: number;
	down: number;
	puts: number;
	gets: number;
	fails: number;
	waitNs: number;
	// Parts got from the owning peer instead of R2, and served to peers,
	// and their bytes.
	peerGiven: number;
	peerServed: number;
	peerGivenBytes: number;
	peerServedBytes: number;
}

function hasObjectStoreCounters(s: SampleRow): boolean {
	return (
		s.r2Up > 0 ||
		s.r2Down > 0 ||
		s.r2Puts > 0 ||
		s.r2Gets > 0 ||
		s.r2Fails > 0 ||
		s.r2WaitNs > 0 ||
		s.peerGiven > 0 ||
		s.peerServed > 0 ||
		s.peerGivenBytes > 0 ||
		s.peerServedBytes > 0
	);
}

function counterDelta(curr: number, prev: number): number {
	return curr >= prev ? curr - prev : curr;
}

export function buildSeries(
	samples: SampleRow[],
	nowMs: number,
): {
	series: Record<string, Samples>;
	load: LoadSample[];
	nodeHourly: Record<string, NodeHours>;
	hourlyR2: Map<number, { up: number; down: number }>;
	r2Totals: R2Totals;
} {
	const live: Record<string, Samples> = {};
	const bucketNodes = new Map<
		number,
		Map<string, { running: number; queued: number; parked: number }>
	>();
	const hours = new Map<
		string,
		Map<number, { cpu: number; mem: number; held: number; n: number; r2Up: number; r2Down: number }>
	>();
	const hourlyR2 = new Map<number, { up: number; down: number }>();
	const prevR2 = new Map<string, SampleRow>();
	const r2Totals: R2Totals = {
		up: 0,
		down: 0,
		puts: 0,
		gets: 0,
		fails: 0,
		waitNs: 0,
		peerGiven: 0,
		peerServed: 0,
		peerGivenBytes: 0,
		peerServedBytes: 0,
	};
	const since = nowMs - LIVE_WINDOW_MS;

	for (const s of samples) {
		let dUp = 0;
		let dDown = 0;
		let dPuts = 0;
		let dGets = 0;
		let dFails = 0;
		let dWaitNs = 0;
		let dPeerGiven = 0;
		let dPeerServed = 0;
		let dPeerGivenBytes = 0;
		let dPeerServedBytes = 0;
		let dtSec = SAMPLE_SECONDS;
		if (hasObjectStoreCounters(s)) {
			const prev = prevR2.get(s.node);
			if (prev !== undefined && s.t > prev.t) {
				dtSec = Math.max(1, (s.t - prev.t) / 1000);
				dUp = counterDelta(s.r2Up, prev.r2Up);
				dDown = counterDelta(s.r2Down, prev.r2Down);
				dPuts = counterDelta(s.r2Puts, prev.r2Puts);
				dGets = counterDelta(s.r2Gets, prev.r2Gets);
				dFails = counterDelta(s.r2Fails, prev.r2Fails);
				dWaitNs = counterDelta(s.r2WaitNs, prev.r2WaitNs);
				dPeerGiven = counterDelta(s.peerGiven, prev.peerGiven);
				dPeerServed = counterDelta(s.peerServed, prev.peerServed);
				dPeerGivenBytes = counterDelta(s.peerGivenBytes, prev.peerGivenBytes);
				dPeerServedBytes = counterDelta(s.peerServedBytes, prev.peerServedBytes);
			}
			prevR2.set(s.node, s);
		}
		r2Totals.up += dUp;
		r2Totals.down += dDown;
		r2Totals.puts += dPuts;
		r2Totals.gets += dGets;
		r2Totals.fails += dFails;
		r2Totals.waitNs += dWaitNs;
		r2Totals.peerGiven += dPeerGiven;
		r2Totals.peerServed += dPeerServed;
		r2Totals.peerGivenBytes += dPeerGivenBytes;
		r2Totals.peerServedBytes += dPeerServedBytes;

		if (s.t >= since) {
			if (!live[s.node]) live[s.node] = create(SamplesSchema);
			live[s.node].samples.push(
				create(SampleSchema, {
					t: BigInt(s.t),
					cpuBusy: s.cpu,
					memoryUsed: s.memory,
					memoryPressure: s.psi,
					cpuPressure: s.cpuPsi,
					holding: s.holding,
					uploadBytesPerSec: dUp / dtSec,
					downloadBytesPerSec: dDown / dtSec,
					commitWaitMsPerSec: dWaitNs / 1e6 / dtSec,
					peerPartsGivenPerSec: dPeerGiven / dtSec,
					peerPartsServedPerSec: dPeerServed / dtSec,
					r2GetsPerSec: dGets / dtSec,
					peerGivenBytesPerSec: dPeerGivenBytes / dtSec,
					peerServedBytesPerSec: dPeerServedBytes / dtSec,
				}),
			);
			const b = Math.floor(s.t / (SAMPLE_SECONDS * 1000)) * (SAMPLE_SECONDS * 1000);
			let bn = bucketNodes.get(b);
			if (!bn) {
				bn = new Map();
				bucketNodes.set(b, bn);
			}
			bn.set(s.node, { running: s.running, queued: s.queued, parked: s.parked });
		}
		let hm = hours.get(s.node);
		if (!hm) {
			hm = new Map();
			hours.set(s.node, hm);
		}
		const h = hourOf(s.t);
		let acc = hm.get(h);
		if (!acc) {
			acc = { cpu: 0, mem: 0, held: 0, n: 0, r2Up: 0, r2Down: 0 };
			hm.set(h, acc);
		}
		acc.cpu += s.cpu;
		acc.mem += s.memory;
		if (s.holding) acc.held++;
		acc.n++;
		acc.r2Up += dUp;
		acc.r2Down += dDown;

		if (dUp > 0 || dDown > 0) {
			let hr = hourlyR2.get(h);
			if (!hr) {
				hr = { up: 0, down: 0 };
				hourlyR2.set(h, hr);
			}
			hr.up += dUp;
			hr.down += dDown;
		}
	}

	const load: LoadSample[] = [];
	for (const [b, nodes] of [...bucketNodes.entries()].sort((a, b) => a[0] - b[0])) {
		const ls = create(LoadSampleSchema, { t: BigInt(b) });
		for (const c of nodes.values()) {
			ls.running += c.running;
			ls.queued += c.queued;
			ls.parked += c.parked;
		}
		load.push(ls);
	}

	const dh = dayHours(nowMs);
	const nodeHourly: Record<string, NodeHours> = {};
	for (const [node, hm] of hours) {
		const nh = create(NodeHoursSchema);
		for (const h of dh) {
			const acc = hm.get(h);
			nh.hours.push(
				create(NodeHourSchema, {
					hour: BigInt(h),
					cpuBusy: acc ? acc.cpu / acc.n : 0,
					memoryUsed: acc ? acc.mem / acc.n : 0,
					holding: acc ? acc.held / acc.n : 0,
					uploadBytes: BigInt(Math.round(acc?.r2Up ?? 0)),
					downloadBytes: BigInt(Math.round(acc?.r2Down ?? 0)),
				}),
			);
		}
		nodeHourly[node] = nh;
	}
	return { series: live, load, nodeHourly, hourlyR2, r2Totals };
}

function buildHourly(
	history: FinishedCommand[],
	forks: ForkRow[],
	nowMs: number,
	hourlyR2: Map<number, { up: number; down: number }>,
): HourBucket[] {
	const dh = dayHours(nowMs);
	const idx = new Map<number, number>();
	const out = dh.map((h, i) => {
		idx.set(h, i);
		const r2 = hourlyR2.get(h);
		return create(HourBucketSchema, {
			hour: BigInt(h),
			uploadBytes: BigInt(Math.round(r2?.up ?? 0)),
			downloadBytes: BigInt(Math.round(r2?.down ?? 0)),
		});
	});
	const waits: number[][] = out.map(() => []);
	for (const c of history) {
		const i = idx.get(hourOf(Number(c.endedAt)));
		if (i === undefined) continue;
		const b = out[i];
		waits[i].push(c.queuedSeconds);
		b.parks += c.parks;
		if (c.coldSeconds === undefined || c.state === CommandState.CANCELLED) continue;
		b.builds++;
		b.spentSeconds += c.seconds;
		b.savedSeconds += Math.max(0, c.coldSeconds - c.seconds);
	}
	for (const f of forks) {
		const i = idx.get(hourOf(f.at));
		if (i !== undefined) out[i].forks++;
	}
	for (let i = 0; i < out.length; i++) {
		out[i].queueWaitP95Seconds = quantile(waits[i], 0.95);
	}
	return out;
}

function countOomKills(samples: SampleRow[]): number {
	const first = new Map<string, number>();
	const last = new Map<string, number>();
	for (const s of samples) {
		if (!first.has(s.node)) first.set(s.node, s.oom);
		const prev = last.get(s.node);
		if (prev !== undefined && s.oom < prev) {
			first.set(s.node, (first.get(s.node) ?? 0) - prev);
		}
		last.set(s.node, s.oom);
	}
	let total = 0;
	for (const [n, l] of last) total += l - (first.get(n) ?? 0);
	return total;
}

function buildTotals(
	history: FinishedCommand[],
	forks: ForkRow[],
	oom: number,
	r2Totals: R2Totals,
): Totals {
	const t = create(TotalsSchema, {
		oomKills: oom,
		forks: forks.length,
		r2UploadBytes: BigInt(Math.round(r2Totals.up)),
		r2DownloadBytes: BigInt(Math.round(r2Totals.down)),
		r2PutRequests: BigInt(Math.round(r2Totals.puts)),
		r2GetRequests: BigInt(Math.round(r2Totals.gets)),
		r2FailedRequests: BigInt(Math.round(r2Totals.fails)),
		r2CommitWaitSeconds: r2Totals.waitNs / 1e9,
		peerPartsGiven: BigInt(Math.round(r2Totals.peerGiven)),
		peerPartsServed: BigInt(Math.round(r2Totals.peerServed)),
		peerGivenBytes: BigInt(Math.round(r2Totals.peerGivenBytes)),
		peerServedBytes: BigInt(Math.round(r2Totals.peerServedBytes)),
	});
	for (const c of history) {
		t.parks += c.parks;
		if (c.coldSeconds === undefined || c.state === CommandState.CANCELLED) continue;
		t.builds++;
		t.savedSeconds += Math.max(0, c.coldSeconds - c.seconds);
	}
	const ms = forks.filter((f) => f.millis >= 0).map((f) => f.millis);
	t.medianForkMillis = BigInt(Math.round(quantile(ms, 0.5)));
	return t;
}

export function createClusterService(
	ctrl: ControllerContext,
	schedules: ScheduleRunner,
): ServiceImpl<typeof ClusterService> {
	function checkClusterId(callerHost: string, clusterId: string): void {
		if (clusterId !== ctrl.clusterId) {
			throw hotbedError(
				ErrorCode.ClusterMismatch,
				`${callerHost} joined cluster ${clusterId}, but this controller runs cluster ${ctrl.clusterId}`,
			).withSuggestion("join this cluster with a new state directory: hotbed join CONTROLLER DIR");
		}
	}

	function mergeEvents(lists: ClusterEvent[][], limit: number): ClusterEvent[] {
		const merged: ClusterEvent[] = [];
		const seen = new Set<string>();
		for (const list of lists) {
			for (const ev of list) {
				const key = `${ev.nodeId}|${ev.at}|${ev.kind}|${ev.text}|${ev.ref?.type ?? ""}|${ev.ref?.id ?? ""}`;
				if (seen.has(key)) continue;
				seen.add(key);
				merged.push(ev);
			}
		}
		merged.sort((a, b) => {
			if (b.at !== a.at) return b.at > a.at ? 1 : -1;
			if (b.id !== a.id) return b.id > a.id ? 1 : -1;
			return a.nodeId < b.nodeId ? -1 : a.nodeId > b.nodeId ? 1 : 0;
		});
		return merged.length > limit ? merged.slice(0, limit) : merged;
	}

	function legacyFilteredEvents(nodeId: string, kinds: string[], limit: number): ClusterEvent[] {
		const all = ctrl.store.fleet.recentEvents(KEEP_EVENTS);
		const hosts = new Map(ctrl.store.fleet.list().map((r) => [r.node.id, r.node.hostname]));
		const out: ClusterEvent[] = [];
		for (const e of all) {
			if (out.length >= limit) break;
			if (nodeId && e.nodeId !== nodeId) continue;
			if (kinds.length > 0 && !kinds.includes(e.kind)) continue;
			e.hostname = hosts.get(e.nodeId) ?? "";
			out.push(e);
		}
		return out;
	}

	async function filteredEvents(
		nodeId: string,
		kinds: string[],
		limit: number,
	): Promise<ClusterEvent[]> {
		const nodes = ctrl.store.fleet.list();
		const targetNodes = nodeId ? nodes.filter((r) => r.node.id === nodeId) : nodes;
		const workerLists = await Promise.all(
			targetNodes.map((r) =>
				ctrl.workers.recentEvents(r.node.id, r.node.hostname, { kinds, limit }),
			),
		);
		const legacy = legacyFilteredEvents(nodeId, kinds, limit);
		return mergeEvents([...workerLists, legacy], limit);
	}

	function filteredPlacements(nodeId: string, session: string, limit: number): PlacementDecision[] {
		const all = ctrl.store.fleet.placementsSince(ctrl.now() - KEEP_PLACEMENTS_MS);
		const out: PlacementDecision[] = [];
		for (const d of all) {
			if (out.length >= limit) break;
			const weighed = !nodeId || d.candidates.some((c) => c.nodeId === nodeId);
			if (weighed && (!session || d.sessionId === session)) {
				out.push(d);
			}
		}
		return out;
	}

	async function ensureCommandOrDiagnosticsExist(id: string): Promise<void> {
		if (await ctrl.commands.hasDiagnostics(id)) return;
		ctrl.store.commands.lookup(id);
	}

	return {
		async join(_req, ctx) {
			const caller = getCaller(ctx.values);
			const nowMs = ctrl.now();
			const node = ctrl.store.fleet.join(caller.nodeId, caller.hostname, caller.login, nowMs);
			let workerToken = "";
			if (caller.authMethod !== "worker-token") {
				const orgId = ctrl.orgId || caller.orgId;
				workerToken = formatOrgToken("hbw", orgId);
				const hash = await sha256Hex(workerToken);
				ctrl.store.tokens.putWorkerToken(hash, node.id, node.hostname, nowMs);
			}
			return create(JoinResponseSchema, {
				clusterId: ctrl.clusterId,
				node: ctrl.withOnline(node),
				workerToken,
			});
		},

		async pushEvents(req, ctx) {
			const caller = getCaller(ctx.values);
			checkClusterId(caller.hostname, req.clusterId);
			if (!ctrl.store.fleet.has(caller.nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `node ${caller.nodeId} has not joined this cluster`);
			}
			// Persist per-command profiles and diagnostics to each command's own
			// CommandDO via typed Workers RPC before advancing node.applied, so a
			// transient RPC failure lets the worker retry without dropping data.
			const commandWrites: Array<Promise<void>> = [];
			for (const ev of req.events) {
				if (ev.change.case !== "command") continue;
				const cc = ev.change.value;
				const cmd = cc.command;
				if (!cmd || cc.what === "forgotten") continue;
				if (cc.profile) {
					commandWrites.push(
						ctrl.commands.saveProfile(
							caller.nodeId,
							Number(cmd.endedAt ?? ev.at),
							cc.profile,
							ctrl.now(),
						),
					);
				}
				if (cc.diagnostics) {
					commandWrites.push(
						ctrl.commands.saveDiagnostics(caller.nodeId, cc.diagnostics, Number(ev.at)),
					);
				}
			}
			if (commandWrites.length > 0) {
				await Promise.all(commandWrites);
			}
			const res = ctrl.store.nodeState.push(
				caller.nodeId,
				req,
				ctrl.now(),
				ctrl.creatingWorkspacesSet(),
			);
			if (res.feedEvents.length > 0) {
				await ctrl.workers.appendEvents(caller.nodeId, caller.hostname, res.feedEvents);
			}
			ctrl.sessionWakes.wakeAll(res.sessions);
			if (res.lost.length > 0) ctrl.loseRuns(res.lost);
			if (res.finished) await ctrl.settleRuns();
			if (res.finished || res.lost.length > 0) {
				ctrl.nodeChanges.wake(caller.hostname);
				ctrl.capacityChanges.wake("*");
			}
			return create(PushEventsResponseSchema, {
				applied: res.applied,
				checkpointNeeded: res.checkpointNeeded,
			});
		},

		async heartbeat(req, ctx) {
			const caller = getCaller(ctx.values);
			checkClusterId(caller.hostname, req.clusterId);
			const nowMs = ctrl.now();
			const sample = ctrl.store.fleet.beat(caller.nodeId, req, nowMs);
			await ctrl.workers.recordSample(caller.nodeId, caller.hostname, sample);
			const holdsBases = reportsHeldBases(req, ctrl.follows(req.resources));
			if (holdsBases) {
				const recorded = ctrl.store.cladefs.recordHeldBases(
					caller.nodeId,
					req.resources?.arch ?? "",
					req.heldBases,
					req.retiredBases,
					nowMs,
					ctrl.cladefsTenantId,
				);
				for (const r of recorded) {
					ctrl.clearBaseCaptureForKey(r.project, r.arch, r.runtime, r.tenant);
				}
			}
			ctrl.nodeChanges.wake(caller.hostname);
			ctrl.capacityChanges.wake("*");
			const { projects, release } = hostingFor(ctrl, caller.nodeId, caller.hostname);
			if (ctrl.builds.needsImmediateRefresh(nowMs)) {
				try {
					await ctrl.builds.ensureLoaded(nowMs);
				} catch (err) {
					ctrl.store.daemonLogs.log(
						DaemonLogLevel.WARN,
						"load controller build release metadata",
						{ err: err instanceof Error ? err.message : String(err) },
						nowMs,
					);
				}
			}
			const build = ctrl.builds.buildForNode(req.resources);
			const peerSet = new Set<string>();
			for (const nr of ctrl.store.fleet.list()) {
				const peerAddr = nr.hb?.cladefsPeerAddr?.trim();
				if (peerAddr && ctrl.withOnline(nr.node).online) {
					peerSet.add(peerAddr);
				}
			}
			const cladefsPeers = [...peerSet].sort();
			const decommissionIncarnations =
				req.cladefsPeerAddr || req.cladefsIncarnation > 0n
					? ctrl.store.cladefs.listPendingDecommissions(nowMs - 24 * 3600_000)
					: [];
			const workerConfig = applyOrgCladefsTenant(
				ctrl.store.workerConfigs.effective(caller.hostname, caller.nodeId),
				ctrl.cladefsTenantId,
				caller.hostname,
			);
			return create(HeartbeatResponseSchema, {
				projects,
				release,
				build,
				cladefsPeers,
				decommissionIncarnations,
				workerConfig,
				retireBases: holdsBases
					? ctrl.store.cladefs.basesToRetire(caller.nodeId, nowMs, SHARED_BASE_RETIRE_GRACE_MS)
					: [],
			});
		},

		listNodes() {
			const rows = ctrl.store.fleet.list();
			return create(ListNodesResponseSchema, {
				clusterId: ctrl.clusterId,
				nodes: rows.map((r) => ctrl.withOnline(r.node)),
				controllerVersion: ctrl.builds.versionString(),
			});
		},

		async getBuild(req) {
			const nowMs = ctrl.now();
			if (ctrl.builds.needsImmediateRefresh(nowMs)) {
				try {
					await ctrl.builds.ensureLoaded(nowMs);
				} catch (err) {
					ctrl.store.daemonLogs.log(
						DaemonLogLevel.WARN,
						"load controller build release metadata",
						{ err: err instanceof Error ? err.message : String(err) },
						nowMs,
					);
				}
			}
			const b = ctrl.builds.buildFor(req.os, req.arch);
			if (b) return b;
			const cur = ctrl.builds.currentBuildNumber();
			if (cur > 0 && isSupportedPlatform(req.os, req.arch)) {
				throw hotbedError(
					ErrorCode.NotFound,
					`the controller runs build ${cur}, which it has not fetched yet`,
				).withSuggestion("try again in a few minutes");
			}
			if (cur > 0) {
				throw hotbedError(ErrorCode.NotFound, `no build is made for ${req.os}/${req.arch}`);
			}
			throw hotbedError(
				ErrorCode.NotFound,
				`the controller runs ${ctrl.builds.versionString()}, a build it doesn't keep: nothing to upgrade to`,
			).withSuggestion("configure UPDATES_REPO on the Cloudflare Worker to follow GitHub releases");
		},

		listRepos(req) {
			let repos = ctrl.store.projects.listRepos();
			if (req.project) {
				const p = ctrl.store.projects.get(req.project);
				repos = repos.filter((r) => p.repos.includes(canonicalRemote(r.url)));
			}
			return create(ListReposResponseSchema, { repos });
		},

		putRepo(req, ctx) {
			const caller = getCaller(ctx.values);
			const repoIn = req.repo;
			if (!repoIn || !REPO_ID_RE.test(repoIn.id)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`repo name "${repoIn?.id ?? ""}" must be lowercase letters, digits and dashes`,
				);
			}
			validateRepoURL(repoIn.url, ctrl.allowLocalRepos);
			if (req.project) ctrl.store.projects.get(req.project);
			const repo = clone(RepoSchema, repoIn);
			repo.createdAt = BigInt(ctrl.now());
			repo.reminder = undefined;
			ctrl.store.projects.putRepo(repo);

			let proj;
			if (req.project) {
				const remote = canonicalRemote(repo.url);
				proj = ctrl.store.projects.add(
					req.project,
					remote ? [remote] : [],
					ctrl.madeBy(caller, `for registered repo ${repo.id}`),
				);
			} else {
				proj = ensureRepoProject(ctrl, caller, repo.id, repo.url);
			}
			repo.reminder = setupStepsReminder(proj);
			return repo;
		},

		deleteRepo(req) {
			if (!ctrl.store.projects.deleteRepo(req.id)) {
				throw hotbedError(ErrorCode.NotFound, `no repo ${req.id}`);
			}
			return create(DeleteRepoResponseSchema);
		},

		rebuildProject(req) {
			return ctrl.store.projects.editSetup(req.name, (p) => {
				p.scratchEpoch += 1n;
			});
		},

		setProjectHosts(req) {
			const hosts = [...new Set(req.hosts)].sort();
			return ctrl.store.projects.editSetup(req.name, (p) => {
				p.hosts = hosts;
			});
		},

		async putStep(req, ctx) {
			const caller = getCaller(ctx.values);
			const st = req.step;
			if (!st || !STEP_NAME_RE.test(st.name) || st.name.length > 40) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`step name "${st?.name ?? ""}" must be lowercase words joined by dashes`,
				);
			}
			if (!st.script.trim()) {
				throw hotbedError(ErrorCode.InvalidArgs, `step ${st.name} has no script`);
			}
			const hash = await stepHash(st.script);
			const step = create(StepSchema, {
				name: st.name,
				script: st.script,
				everyPull: st.everyPull,
				hash,
				updatedAt: BigInt(ctrl.now()),
				updatedBy: caller.hostname,
			});
			return ctrl.store.projects.editSetup(req.project, (p) => {
				if (req.ifRevision > 0n && p.setupRevision !== req.ifRevision) {
					throw hotbedError(
						ErrorCode.Conflict,
						`project ${p.name} setup is at revision ${p.setupRevision}, not ${req.ifRevision}`,
					).withSuggestion(`hotbed step ls -p ${p.name}`);
				}
				const at = p.steps.findIndex((o) => o.name === step.name);
				if (req.before === step.name) {
					throw hotbedError(ErrorCode.InvalidArgs, `step ${step.name} cannot go before itself`);
				}
				if (req.before) {
					if (at >= 0) p.steps.splice(at, 1);
					const beforeIdx = p.steps.findIndex((o) => o.name === req.before);
					if (beforeIdx < 0) {
						throw hotbedError(
							ErrorCode.NotFound,
							`project ${p.name} has no step ${req.before} to put ${step.name} before`,
						);
					}
					p.steps.splice(beforeIdx, 0, step);
				} else if (at >= 0) {
					p.steps[at] = step;
				} else {
					p.steps.push(step);
				}
			});
		},

		deleteStep(req) {
			return ctrl.store.projects.editSetup(req.project, (p) => {
				if (req.ifRevision > 0n && p.setupRevision !== req.ifRevision) {
					throw hotbedError(
						ErrorCode.Conflict,
						`project ${p.name} setup is at revision ${p.setupRevision}, not ${req.ifRevision}`,
					).withSuggestion(`hotbed step ls -p ${p.name}`);
				}
				const at = p.steps.findIndex((o) => o.name === req.name);
				if (at < 0) {
					throw hotbedError(
						ErrorCode.NotFound,
						`project ${p.name} has no step ${req.name}`,
					).withSuggestion(`hotbed step ls -p ${p.name}`);
				}
				p.steps.splice(at, 1);
			});
		},

		listSteps(req) {
			const proj = ctrl.store.projects.get(req.project);
			const nvs = ctrl.views();
			const nodes: Array<{ node: string; base?: Base }> = [];
			for (const nv of nvs) {
				if (proj.hosts.length > 0 && !proj.hosts.includes(nv.node.hostname)) continue;
				let newest: Base | undefined;
				for (const b of nv.bases) {
					if (
						b.project === proj.name &&
						b.state !== BaseState.BUILDING &&
						(!newest || b.generation > newest.generation)
					) {
						newest = b;
					}
				}
				nodes.push({ node: nv.node.hostname, base: newest });
			}
			const steps = proj.steps.map((step) =>
				create(StepStatusSchema, {
					step,
					nodes: nodes.map((n) => {
						const on = create(StepOnNodeSchema, {
							node: n.node,
							baseId: n.base?.id ?? "",
							state: StepState.PENDING,
						});
						for (const a of n.base?.steps ?? []) {
							if (a.name !== step.name || a.hash !== step.hash) continue;
							if (a.ok) {
								on.state = StepState.APPLIED;
								on.seconds = a.seconds;
							} else if (a.ran) {
								on.state = StepState.FAILED;
							}
						}
						return on;
					}),
				}),
			);
			return create(ListStepsResponseSchema, { steps });
		},

		addProject(req, ctx) {
			checkProjectName(req.name);
			const remotes = canonicalRemotes(req.repos);
			if (!req.shared) {
				for (const r of remotes) {
					const others = ctrl.store.projects
						.holding(r)
						.filter((p) => p.name !== req.name)
						.map((p) => p.name);
					if (others.length > 0) {
						throw hotbedError(
							ErrorCode.Conflict,
							`${r} is in project ${others.join(", ")} already: in a second project, every session starting in its checkouts would have to name its project (-p)`,
						).withSuggestion(
							`pass --shared to add it anyway, or take it out of the other first: hotbed project rm-repo ${others[0]} --repo ${r}`,
						);
					}
				}
			}
			const { by } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const p = ctrl.store.projects.add(req.name, remotes, by);
			p.reminder = setupStepsReminder(p);
			return p;
		},

		removeProjectRepos(req, ctx) {
			if (req.repos.length === 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`name the repos to take out of project ${req.name}`,
				).withSuggestion("hotbed project rm-repo NAME --repo REMOTE");
			}
			const remotes = canonicalRemotes(req.repos);
			const { by } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.projects.removeRepos(req.name, remotes, by);
		},

		listProjects() {
			return create(ListProjectsResponseSchema, { projects: projectsWithCounts(ctrl) });
		},

		getProject(req) {
			const ps = projectsWithCounts(ctrl);
			const match = ps.find((p) => p.name === req.name);
			if (!match) {
				throw hotbedError(ErrorCode.NotFound, `no project ${req.name}`).withSuggestion(
					"hotbed project ls",
				);
			}
			match.history = ctrl.store.projects.history(req.name);
			return match;
		},

		listBases(req) {
			if (
				req.state !== BaseState.UNSPECIFIED &&
				req.state !== BaseState.BUILDING &&
				req.state !== BaseState.WARM &&
				req.state !== BaseState.STALE &&
				req.state !== BaseState.FAILED
			) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`unknown base state ${String(req.state)}`,
				).withSuggestion("use one of: building, warm, stale, failed");
			}
			const target = req.project.trim();
			let matchProjects: Set<string> | null = null;
			if (target) {
				matchProjects = new Set<string>();
				try {
					const p = ctrl.store.projects.get(target);
					matchProjects.add(p.name);
				} catch (err) {
					if (codeOf(err) !== ErrorCode.NotFound) throw err;
					try {
						const r = repoById(ctrl, target);
						for (const hp of ctrl.store.projects.holding(canonicalRemote(r.url))) {
							matchProjects.add(hp.name);
						}
					} catch (rerr) {
						if (codeOf(rerr) !== ErrorCode.NotFound) throw rerr;
					}
				}
			}
			const nvs = ctrl.views();
			if (target && matchProjects && matchProjects.size === 0) {
				if (nvs.some((nv) => nv.bases.some((b) => b.project === target))) {
					matchProjects.add(target);
				} else {
					throw hotbedError(ErrorCode.NotFound, `no project ${target}`).withSuggestion(
						"hotbed project ls",
					);
				}
			}
			let nodeId = "";
			if (req.node) {
				const found = nvs.find((nv) => nv.node.hostname === req.node || nv.node.id === req.node);
				if (!found) {
					throw hotbedError(ErrorCode.NotFound, `no node is called ${req.node}`).withSuggestion(
						"hotbed nodes lists the nodes",
					);
				}
				nodeId = found.node.id;
			}
			const bases = [];
			for (const nv of nvs) {
				if (nodeId && nv.node.id !== nodeId) continue;
				for (const b of nv.bases) {
					if (matchProjects && !matchProjects.has(b.project)) continue;
					if (req.state !== BaseState.UNSPECIFIED && b.state !== req.state) continue;
					let failedStep = "";
					if (b.state === BaseState.FAILED) {
						const st = b.steps.find((s) => s.ran && !s.ok);
						if (st) failedStep = st.name;
					}
					bases.push(create(NodeBaseSchema, { node: nv.node.hostname, base: b, failedStep }));
				}
			}
			bases.sort((a, b) => {
				const ap = a.base?.project ?? "";
				const bp = b.base?.project ?? "";
				if (ap !== bp) return ap < bp ? -1 : 1;
				if (a.node !== b.node) return a.node < b.node ? -1 : 1;
				const ag = a.base?.generation ?? 0;
				const bg = b.base?.generation ?? 0;
				if (bg !== ag) return bg - ag;
				const ai = a.base?.id ?? "";
				const bi = b.base?.id ?? "";
				return ai < bi ? -1 : ai > bi ? 1 : 0;
			});
			return create(ListBasesResponseSchema, { bases });
		},

		listFreshens(req) {
			const nowMs = ctrl.now();
			const sinceMs = parseFreshenSinceMs(req.since, nowMs);
			const validStates = new Set<FreshenState>([
				FreshenState.UNSPECIFIED,
				FreshenState.QUEUED,
				FreshenState.RUNNING,
				FreshenState.PARKED,
				FreshenState.PAUSED,
				FreshenState.FINALIZING,
				FreshenState.SUCCEEDED,
				FreshenState.FAILED,
				FreshenState.CANCELLED,
				FreshenState.DROPPED,
			]);
			if (!validStates.has(req.state)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`unknown freshen state ${String(req.state)}`,
				).withSuggestion(
					"use one of: queued, running, parked, paused, finalizing, succeeded, failed, cancelled, dropped",
				);
			}
			const rt = req.runtime.trim().toLowerCase();
			if (rt && rt !== "vm" && rt !== "cladefs") {
				throw hotbedError(ErrorCode.InvalidArgs, `unknown runtime "${req.runtime}"`).withSuggestion(
					"use one of: vm, cladefs",
				);
			}

			const nvs = ctrl.views();
			const target = req.project.trim();
			let matchProjects: Set<string> | null = null;
			if (target) {
				matchProjects = new Set<string>();
				try {
					const p = ctrl.store.projects.get(target);
					matchProjects.add(p.name);
				} catch (err) {
					if (codeOf(err) !== ErrorCode.NotFound) throw err;
					try {
						const r = repoById(ctrl, target);
						for (const hp of ctrl.store.projects.holding(canonicalRemote(r.url))) {
							matchProjects.add(hp.name);
						}
					} catch (rerr) {
						if (codeOf(rerr) !== ErrorCode.NotFound) throw rerr;
					}
				}
			}

			let nodeId = "";
			if (req.node) {
				const found = nvs.find((nv) => nv.node.hostname === req.node || nv.node.id === req.node);
				if (!found) {
					throw hotbedError(ErrorCode.NotFound, `no node is called ${req.node}`).withSuggestion(
						"hotbed nodes lists the nodes",
					);
				}
				nodeId = found.node.id;
			}

			const all = withLiveState(ctrl.store.freshens.list(sinceMs), nvs, nowMs);

			if (target && matchProjects && matchProjects.size === 0) {
				if (all.some((f) => f.project === target)) {
					matchProjects.add(target);
				} else {
					throw hotbedError(ErrorCode.NotFound, `no project ${target}`).withSuggestion(
						"hotbed project ls",
					);
				}
			}

			const matched = all.filter((f) => {
				if (sinceMs > 0 && !isFreshenActive(f.state)) {
					const ts = Number(f.endedAt ?? f.createdAt);
					if (ts > 0 && ts < sinceMs) return false;
				}
				if (matchProjects && !matchProjects.has(f.project)) return false;
				if (nodeId && f.nodeId !== nodeId) return false;
				if (req.state !== FreshenState.UNSPECIFIED && f.state !== req.state) return false;
				if (req.active && !isFreshenActive(f.state)) return false;
				if (rt && f.runtime !== rt) return false;
				if (req.cold && !f.cold) return false;
				return true;
			});

			// Active freshens first (longest running/waiting first), then ended newest-first.
			matched.sort((a, b) => {
				const aAct = isFreshenActive(a.state) ? 1 : 0;
				const bAct = isFreshenActive(b.state) ? 1 : 0;
				if (bAct !== aAct) return bAct - aAct;
				if (aAct && bAct) {
					if (b.totalSeconds !== a.totalSeconds) return b.totalSeconds - a.totalSeconds;
				}
				if (b.createdAt !== a.createdAt) return b.createdAt > a.createdAt ? 1 : -1;
				return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
			});

			const summary = buildFreshenSummary(matched, sinceMs);
			const lim = req.limit === undefined ? DEFAULT_FRESHEN_LIMIT : req.limit;
			let items = matched;
			let more = 0;
			if (lim > 0 && matched.length > lim) {
				items = matched.slice(0, lim);
				more = matched.length - lim;
			}
			return create(ListFreshensResponseSchema, {
				summary,
				freshens: items,
				more,
				now: BigInt(nowMs),
			});
		},

		async getFreshen(req) {
			const id = req.id.trim();
			if (!id) {
				throw hotbedError(ErrorCode.InvalidArgs, "freshen command ID or base ID is required");
			}
			const nowMs = ctrl.now();
			const nvs = ctrl.views();
			let nodeId = "";
			if (req.node) {
				const found = nvs.find((nv) => nv.node.hostname === req.node || nv.node.id === req.node);
				if (!found) {
					throw hotbedError(ErrorCode.NotFound, `no node is called ${req.node}`).withSuggestion(
						"hotbed nodes lists the nodes",
					);
				}
				nodeId = found.node.id;
			}
			const onNode = (f: BaseFreshen) => !nodeId || f.nodeId === nodeId;
			const byId = ctrl.store.freshens.get(id);
			let hits = byId && onNode(byId) ? [byId] : [];
			if (hits.length === 0) hits = ctrl.store.freshens.ofBase(id).filter(onNode);
			if (hits.length === 0) {
				throw hotbedError(ErrorCode.NotFound, `no base freshen ${id}`).withSuggestion(
					"hotbed freshen ls lists recent and active base freshens",
				);
			}
			if (hits.length > 1) {
				const nodes = [...new Set(hits.map((h) => h.node || h.nodeId))].sort();
				if (nodes.length > 1) {
					throw hotbedError(
						ErrorCode.Conflict,
						`base ${id} has freshens on multiple nodes (${nodes.join(", ")})`,
					).withSuggestion(`pass --node HOST, or use the freshen command ID (e.g. ${hits[0].id})`);
				}
			}
			const [freshen] = withLiveState([hits[0]], nvs, nowMs);
			const nv = nvs.find((v) => v.node.id === freshen.nodeId);
			const nodeAdmission =
				nv && nv.online && isFreshenActive(freshen.state) ? nv.node.admission : undefined;

			const hostById = new Map(nvs.map((v) => [v.node.id, v.node.hostname]));
			const host = hostById.get(freshen.nodeId) ?? freshen.node;
			const genPhrase =
				freshen.generation > 0 ? `base ${freshen.project} g${freshen.generation} ` : "";
			const workerEvs = await ctrl.workers.recentEvents(freshen.nodeId, host, { limit: 400 });
			const legacyEvs: ClusterEvent[] = [];
			for (const ev of ctrl.store.fleet.nodeEventsNewestFirst(freshen.nodeId)) {
				ev.hostname = host;
				legacyEvs.push(ev);
				if (legacyEvs.length >= 400) break;
			}
			const candidates = mergeEvents([workerEvs, legacyEvs], 400);
			const related: ClusterEvent[] = [];
			for (const ev of candidates) {
				const matches =
					(ev.ref?.id && (ev.ref.id === freshen.id || ev.ref.id === freshen.baseId)) ||
					(freshen.baseId && ev.text.includes(freshen.baseId)) ||
					(genPhrase && ev.text.includes(genPhrase));
				if (!matches) continue;
				ev.hostname = host;
				related.push(ev);
				if (related.length >= 40) break;
			}

			return create(GetFreshenResponseSchema, {
				freshen,
				nodeAdmission,
				relatedEvents: related,
			});
		},

		async getSnapshot() {
			const nowMs = ctrl.now();
			const dayMs = nowMs - 24 * 3600_000;
			const nvs = ctrl.views();
			const repos = ctrl.store.projects.listRepos();
			const history = ctrl.store.commands.historySince(dayMs);
			const [workerSamplesLists, workerEventLists, workerLogSummaries] = await Promise.all([
				Promise.all(nvs.map((nv) => ctrl.workers.samplesSince(nv.node.id, dayMs))),
				Promise.all(
					nvs.map((nv) =>
						ctrl.workers.recentEvents(nv.node.id, nv.node.hostname, {
							limit: Math.max(EVENTS_SHOWN, ADMISSION_EVENTS_SHOWN),
						}),
					),
				),
				Promise.all(nvs.map((nv) => ctrl.workers.logSummary(nv.node.id, nv.node.hostname, nowMs))),
			]);
			const legacySamples = ctrl.store.fleet.samplesSince(dayMs);
			const sampleMap = new Map<string, SampleRow>();
			for (const s of legacySamples) sampleMap.set(`${s.node}|${s.t}`, s);
			for (const list of workerSamplesLists) {
				for (const s of list) sampleMap.set(`${s.node}|${s.t}`, s);
			}
			const samples = [...sampleMap.values()].sort((a, b) => a.t - b.t);
			const legacyEvents = ctrl.store.fleet.recentEvents(KEEP_EVENTS);
			const hosts = new Map(nvs.map((nv) => [nv.node.id, nv.node.hostname]));
			for (const e of legacyEvents) e.hostname = hosts.get(e.nodeId) ?? "";
			const allRecentEvents = mergeEvents(
				[...workerEventLists, legacyEvents],
				Math.max(EVENTS_SHOWN, ADMISSION_EVENTS_SHOWN),
			);
			const events = allRecentEvents.slice(0, EVENTS_SHOWN);
			const { series, load, nodeHourly, hourlyR2, r2Totals } = buildSeries(samples, nowMs);
			const oom = countOomKills(samples);
			const forks = ctrl.store.workspaces.forksSince(dayMs);
			const sessions = ctrl.store.sessions.recent(dayMs, "", "");
			const scheds = schedules.allSchedules("");
			const openQuestions = ctrl.store.questions.openCount();
			const memCounts = ctrl.store.memories.pendingCounts();
			const projects = projectsWithCounts(ctrl);

			const nodes = [];
			const bases: Base[] = [];
			const workspaces = [];
			const commands: Command[] = [];
			for (let i = 0; i < nvs.length; i++) {
				const nv = nvs[i];
				const node = nv.node;
				if (!nv.online) {
					node.usage = create(NodeUsageSchema);
					node.admission = undefined;
				}
				const logSum = workerLogSummaries[i];
				if (logSum?.hasStreamedLogs) {
					node.daemonLogs = logSum.source;
				}
				nodes.push(node);
				bases.push(...nv.bases);
				workspaces.push(...nv.workspaces);
				if (nv.online) {
					for (const c of nv.commands) {
						if (c.kind !== "freshen" || isCommandLive(c.state)) commands.push(c);
					}
				}
			}
			commands.sort((a, b) => (a.queuedAt < b.queuedAt ? -1 : a.queuedAt > b.queuedAt ? 1 : 0));

			const placements = ctrl.store.fleet.placementsSince(dayMs).slice(0, PLACEMENTS_SHOWN);
			const admissionEvents = allRecentEvents
				.filter((e) => ADMISSION_KINDS.includes(e.kind))
				.slice(0, ADMISSION_EVENTS_SHOWN);

			const openBoardTasks = projects.reduce((sum, p) => sum + p.openBoardTasks, 0);
			const blockedBoardTasks = projects.reduce((sum, p) => sum + p.blockedBoardTasks, 0);

			return create(SnapshotSchema, {
				clusterId: ctrl.clusterId,
				controller: ctrl.controllerName,
				now: BigInt(nowMs),
				sampleSeconds: SAMPLE_SECONDS,
				nodes,
				repos: repos.map((r) => summarizeRepo(r, bases)),
				bases,
				workspaces,
				commands,
				events,
				history,
				series,
				load,
				hourly: buildHourly(history, forks, nowMs, hourlyR2),
				nodeHourly,
				totals24h: buildTotals(history, forks, oom, r2Totals),
				sessions,
				schedules: scheds,
				openQuestions,
				projects,
				controllerLogs: ctrl.store.daemonLogs.summary(ctrl.controllerName, nowMs),
				placements,
				admissionEvents,
				pendingMemories: memCounts.total,
				openIssues: ctrl.store.issues.openCount(),
				globalPendingMemories: memCounts.global,
				openBoardTasks,
				blockedBoardTasks,
			});
		},

		async saveCommandDiagnostics(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!ctrl.store.fleet.has(caller.nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `${caller.hostname} has not joined this cluster`);
			}
			const d = req.diagnostics;
			if (!d) throw hotbedError(ErrorCode.InvalidArgs, "diagnostics is required");
			if (!d.commandId.trim()) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			if (!d.intervention || d.intervention.seq <= 0) {
				throw hotbedError(ErrorCode.InvalidArgs, "intervention with positive seq is required");
			}
			await ctrl.commands.saveDiagnostics(caller.nodeId, d, ctrl.now());
			return create(SaveCommandDiagnosticsResponseSchema);
		},

		async saveCommandSample(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!ctrl.store.fleet.has(caller.nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `${caller.hostname} has not joined this cluster`);
			}
			const s = req.sample;
			if (!s) throw hotbedError(ErrorCode.InvalidArgs, "sample is required");
			if (!s.commandId.trim()) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			if (s.seq <= 0)
				throw hotbedError(ErrorCode.InvalidArgs, "sample with positive seq is required");
			await ctrl.commands.saveSample(caller.nodeId, s, ctrl.now());
			return create(SaveCommandSampleResponseSchema);
		},

		async pushCommandOutput(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!ctrl.store.fleet.has(caller.nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `${caller.hostname} has not joined this cluster`);
			}
			const commandId = req.commandId.trim();
			if (!commandId) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			let totalBytes = 0;
			for (const chunk of req.chunks) {
				if (chunk.offset < 0n || chunk.nextOffset < chunk.offset) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`invalid chunk offsets [${chunk.offset}, ${chunk.nextOffset})`,
					);
				}
				totalBytes += chunk.data.byteLength;
			}
			if (totalBytes > 2 << 20) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`pushCommandOutput batch too large (${totalBytes} bytes, max 2 MiB)`,
				);
			}
			const { ackedOffset } = await ctrl.commands.appendOutputChunks(
				commandId,
				req.chunks,
				req.eof,
				ctrl.now(),
			);
			return create(PushCommandOutputResponseSchema, { ackedOffset: BigInt(ackedOffset) });
		},

		async pushDaemonLogs(req, ctx) {
			const caller = getCaller(ctx.values);
			if (req.clusterId) {
				checkClusterId(caller.hostname, req.clusterId);
			}
			if (!ctrl.store.fleet.has(caller.nodeId)) {
				throw hotbedError(ErrorCode.NotJoined, `${caller.hostname} has not joined this cluster`);
			}
			if (req.entries.length > 1000) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`pushDaemonLogs batch too large (${req.entries.length} entries, max 1000)`,
				);
			}
			const { ackedSeq } = await ctrl.workers.appendLogs(
				caller.nodeId,
				caller.hostname,
				req.epoch,
				req.entries,
				ctrl.now(),
			);
			return create(PushDaemonLogsResponseSchema, { ackedSeq: BigInt(ackedSeq) });
		},

		async listCommandSamples(req) {
			if (!req.id.trim()) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			const samples = await ctrl.commands.listSamples(req.id);
			if (samples.length === 0) ctrl.store.commands.lookup(req.id);
			return create(ListCommandSamplesResponseSchema, { samples });
		},

		async getCommandSample(req) {
			if (!req.id.trim()) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			if (req.seq <= 0) throw hotbedError(ErrorCode.InvalidArgs, "seq must be positive");
			const s = await ctrl.commands.getSample(req.id, req.seq);
			if (s) return s;
			ctrl.store.commands.lookup(req.id);
			throw hotbedError(
				ErrorCode.NotFound,
				`command ${req.id} has no sample ${req.seq}`,
			).withSuggestion(`list its samples (hotbed samples ls ${req.id})`);
		},

		async getCommandDiagnostics(req) {
			if (!req.id.trim()) throw hotbedError(ErrorCode.InvalidArgs, "command ID is required");
			if (req.seq < 0) throw hotbedError(ErrorCode.InvalidArgs, "seq must be non-negative");
			if (req.afterSeq < 0)
				throw hotbedError(ErrorCode.InvalidArgs, "after_seq must be non-negative");
			if (req.seq > 0) {
				const d = await ctrl.commands.getDiagnostic(req.id, req.seq);
				if (d) return create(GetCommandDiagnosticsResponseSchema, { diagnostics: [d] });
				await ensureCommandOrDiagnosticsExist(req.id);
				return create(GetCommandDiagnosticsResponseSchema);
			}
			const ds = await ctrl.commands.listDiagnosticsAfter(req.id, req.afterSeq);
			if (ds.length === 0) {
				await ensureCommandOrDiagnosticsExist(req.id);
				return create(GetCommandDiagnosticsResponseSchema);
			}
			const page: CommandDiagnostics[] = [];
			let totalBytes = 0;
			let hasMore = false;
			for (const d of ds) {
				const sz = toBinary(CommandDiagnosticsSchema, d).byteLength;
				if (page.length > 0 && totalBytes + sz > MAX_DIAGNOSTICS_RESPONSE_BYTES) {
					hasMore = true;
					break;
				}
				page.push(d);
				totalBytes += sz;
			}
			return create(GetCommandDiagnosticsResponseSchema, {
				diagnostics: page,
				hasMore,
				nextAfterSeq:
					hasMore && page.length > 0 ? (page[page.length - 1].intervention?.seq ?? 0) : 0,
			});
		},

		async getNode(req) {
			const r = ctrl.findNodeByNameOrId(req.node);
			const n = ctrl.withOnline(r.node);
			if (!n.online) n.admission = undefined;
			const limit = req.limit || 30;
			return create(NodeDetailSchema, {
				node: n,
				events: await filteredEvents(n.id, ADMISSION_KINDS, limit),
				placements: filteredPlacements(n.id, "", limit),
			});
		},

		async retireNode(req) {
			const r = ctrl.findNodeByNameOrId(req.node);
			const n = ctrl.withOnline(r.node);
			if (n.online) {
				throw hotbedError(
					ErrorCode.Conflict,
					`${n.hostname} is online: only a node gone for good is retired`,
				).withSuggestion("stop its worker first: hotbed worker stop (or destroy) on its machine");
			}
			const nowMs = ctrl.now();
			ctrl.store.fleet.retire(n.id, nowMs);
			await ctrl.workers.markRetired(n.id, nowMs);
			const inc = r.hb?.cladefsIncarnation ?? 0n;
			if (inc > 0n) {
				ctrl.store.cladefs.recordRetiredIncarnation(inc, n.id, n.hostname, nowMs);
			}
			return n;
		},

		listPlacements(req) {
			const nodeId = req.node ? ctrl.findNodeByNameOrId(req.node).node.id : "";
			return create(ListPlacementsResponseSchema, {
				placements: filteredPlacements(nodeId, req.session, req.limit || 50),
			});
		},

		async listEvents(req) {
			const nodeId = req.node ? ctrl.findNodeByNameOrId(req.node).node.id : "";
			return create(ListEventsResponseSchema, {
				events: await filteredEvents(nodeId, req.kinds, req.limit || 100),
			});
		},

		getWorkerConfig(req, ctx) {
			const target = req.node.trim();
			const caller = getCaller(ctx.values);
			if (caller.role === "worker" && (target === "" || target !== caller.hostname)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					`a worker may read only its own config (${caller.hostname}), not ${target || "every scope"}`,
				);
			}
			const defaults = applyOrgCladefsTenant(
				ctrl.store.workerConfigs.get("") ?? undefined,
				ctrl.cladefsTenantId,
				"",
			);
			if (!target) {
				const nodes = ctrl.store.workerConfigs.list().filter((c) => c.node !== "");
				return create(GetWorkerConfigResponseSchema, { defaults, nodes });
			}
			const override = ctrl.store.workerConfigs.get(target);
			const effective = applyOrgCladefsTenant(
				ctrl.store.workerConfigs.effective(target),
				ctrl.cladefsTenantId,
				target,
			);
			return create(GetWorkerConfigResponseSchema, {
				defaults,
				nodes: override ? [override] : [],
				effective,
			});
		},

		setWorkerConfig(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					caller.role === "worker"
						? "workers may not modify organization configuration"
						: "only organization admins may modify organization configuration",
				);
			}
			const scope = req.node.trim();
			const patch = req.config ?? create(WorkerConfigSchema);
			assertNoTenantOverride(patch, ctrl.cladefsTenantId);
			const saved = ctrl.store.workerConfigs.set(scope, patch, {
				replace: req.replace,
				ifRevision: req.ifRevision,
				updatedBy: caller.hostname || caller.login || "unknown",
				nowMs: ctrl.now(),
			});
			return applyOrgCladefsTenant(saved, ctrl.cladefsTenantId, scope) ?? saved;
		},

		deleteWorkerConfig(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					caller.role === "worker"
						? "workers may not modify organization configuration"
						: "only organization admins may modify organization configuration",
				);
			}
			const target = req.node.trim();
			if (req.defaults && target) {
				throw hotbedError(ErrorCode.InvalidArgs, "pass a node name or --defaults, not both");
			}
			if (!req.defaults && !target) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"pass a node name to remove its overrides, or --defaults to remove organization defaults",
				);
			}
			const scope = req.defaults ? "" : target;
			const deleted = ctrl.store.workerConfigs.delete(scope);
			if (!deleted) {
				const label = scope || "defaults";
				throw hotbedError(ErrorCode.NotFound, `no worker config set for ${label}`);
			}
			return create(DeleteWorkerConfigResponseSchema, { node: scope, deleted: true });
		},

		async upgradeSystem(req, ctx) {
			const caller = getCaller(ctx.values);
			if (!isOrgAdminCaller(caller)) {
				throw hotbedError(
					ErrorCode.NotAuthorized,
					caller.role === "worker"
						? "workers may not trigger system upgrades"
						: "only organization admins may trigger system upgrades",
				);
			}
			const target = req.node.trim();
			const targets = [];
			if (target) {
				const r = ctrl.findNodeByNameOrId(target);
				const n = ctrl.withOnline(r.node);
				if (!n.online) {
					throw hotbedError(
						ErrorCode.WorkerUnreachable,
						`${n.hostname} is offline; cannot run system upgrade`,
						{ retryable: true },
					);
				}
				targets.push(n);
			} else {
				const nvs = ctrl.views().filter((nv) => nv.online);
				nvs.sort((a, b) => a.node.hostname.localeCompare(b.node.hostname));
				for (const nv of nvs) {
					targets.push(nv.node);
				}
				if (targets.length === 0) {
					throw hotbedError(ErrorCode.WorkerUnreachable, "no online workers to upgrade", {
						retryable: true,
					});
				}
			}

			const results: NodeSystemUpgrade[] = [];
			for (const node of targets) {
				const arch = node.resources?.arch || "";
				const effective = ctrl.store.workerConfigs.effective(node.hostname, node.id);
				const sys = effective?.system;
				const vm = effective?.vm;
				const cladefsd = arch ? sys?.cladefsd[arch] : undefined;
				const firecracker = arch ? sys?.firecracker[arch] : undefined;
				const kernel = arch ? vm?.kernels[arch] : undefined;

				if (!cladefsd && !firecracker && !kernel && !req.force) {
					results.push(
						create(NodeSystemUpgradeSchema, {
							node: node.hostname,
							arch,
							status: "skipped",
							error: `no system/cladefs artifacts configured for ${arch || "unknown arch"}`,
						}),
					);
					continue;
				}

				const workerReq = create(WorkerUpgradeSystemRequestSchema, {
					cladefsd,
					firecracker,
					kernel,
					cladefsdBin: sys?.cladefsdBin ?? "",
					cladefsdService: sys?.cladefsdService ?? "",
					firecrackerBin: sys?.firecrackerBin ?? vm?.firecrackerBin ?? "",
					force: req.force,
					dryRun: req.dryRun,
				});

				try {
					const res = await ctrl.tunnel.callUnary(
						node.id,
						node.hostname,
						"/hotbed.v1.WorkspaceService/UpgradeSystem",
						WorkerUpgradeSystemRequestSchema,
						workerReq,
						NodeSystemUpgradeSchema,
						5 * 60_000,
						ctx.signal,
					);
					if (!res.node) res.node = node.hostname;
					if (!res.arch) res.arch = arch;
					results.push(res);

					if (!req.dryRun && res.status === "upgraded") {
						const parts: string[] = [];
						if (res.cladefsdUpdated) {
							parts.push(`cladefsd=${res.cladefsdVersion || res.cladefsdSha256.slice(0, 12)}`);
						}
						if (res.firecrackerUpdated) {
							parts.push(
								`firecracker=${res.firecrackerVersion || res.firecrackerSha256.slice(0, 12)}`,
							);
						}
						if (res.kernelUpdated) {
							parts.push(`kernel=${res.kernelVersion || res.kernelSha256.slice(0, 12)}`);
						}
						if (res.restartedCladefsd) {
							parts.push("restarted cladefsd");
						}
						await ctrl.workers.appendEvents(node.id, node.hostname, [
							{
								at: ctrl.now(),
								kind: "upgrade",
								text: `${node.hostname} system upgrade: ${parts.join(", ") || "complete"}`,
								refType: "",
								refId: "",
							},
						]);
					}
					if (res.status === "failed") {
						throw hotbedError(
							ErrorCode.Internal,
							`system upgrade failed on ${node.hostname}: ${res.error || "unknown error"}`,
						);
					}
				} catch (err) {
					const msg = err instanceof Error ? err.message : String(err);
					if (targets.length === 1) {
						throw err;
					}
					throw hotbedError(
						ErrorCode.Internal,
						`rolling system upgrade stopped on ${node.hostname} after ${results.length} node(s): ${msg}`,
					);
				}
			}

			return create(UpgradeSystemResponseSchema, { nodes: results });
		},
	};
}
