import { clone, create } from "@bufbuild/protobuf";
import {
	BaseState,
	CommandState,
	CostPartSchema,
	HeartbeatRequestSchema,
	PlacementCandidateSchema,
	PlacementDecisionSchema,
	PlacementSchema,
	type Admission,
	type Base,
	type Command,
	type CostPart,
	type HeartbeatRequest,
	type HostUsage,
	type Node,
	type Placement,
	type PlacementDecision,
	type Workspace,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, HotbedError, hotbedError } from "./errors.ts";
import { parseBuildNumber } from "./gitops.ts";
import { hostStarvationCost, mergeHostAdmission } from "./hostusage.ts";

const MOVE_COST = 0.3;
const COLD_COST = 0.5;
const FULL_COST = 1;
const BROKEN_COST = 10;
const PRESSURE_COST = 1;
const HOLDING_COST = 1;
const WAKE_COST = 1;
const RECONNECTING_COST = 5;
export const FULL_RAM_FRAC = 0.95; // mirrors internal/worker/engine.go fullAt
export const RACE_COST_MARGIN = 0.35;

export interface NodeRow {
	node: Node;
	addr: string;
	hb?: HeartbeatRequest;
}

export interface NodeView extends NodeRow {
	online: boolean;
	/** Whether the node currently holds an open /ws/worker reverse-RPC tunnel (defaults true when omitted). */
	tunnelConnected?: boolean;
	wakeFailed: boolean;
	bases: Base[];
	workspaces: Workspace[];
	commands: Command[];
}

export interface Candidate {
	view: NodeView;
	host: string;
	mine: string;
	warm: boolean;
	broken: boolean;
	asleep: boolean;
	tunnelConnected: boolean;
	upgrading: number;
	cpus: number;
	cpuBusy: number;
	queued: number;
	parked: number;
	ram: bigint;
	free: bigint;
	pressure: number;
	admission?: Admission;
	hostUsage?: HostUsage;
}

function candidateHolds(c: Candidate, p?: Placement): boolean {
	if (!c.admission?.holding) return false;
	for (const s of c.admission.signals) {
		if (s.holding && (s.name !== "cpu-pressure" || !p?.bench)) {
			return true;
		}
	}
	return false;
}

function candidateMemoryNeed(c: Candidate, p?: Placement): bigint {
	const memLow = p?.memoryLowBytes ?? 0n;
	if (memLow > 0n) return memLow;
	const memEst = p?.memoryEstimateBytes ?? 0n;
	if (memEst > 0n) {
		return c.ram > 0n && memEst > c.ram ? c.ram : memEst;
	}
	return 0n;
}

export function candidateHasCapacity(c: Candidate, p?: Placement): boolean {
	if (c.asleep) return true; // waking an asleep node brings fresh capacity
	if (!c.tunnelConnected) return false;
	if (candidateHolds(c, p)) return false;
	if (c.parked > 0) return false;
	if (c.ram > 0n) {
		if (c.free <= 0n) return false;
		const used = c.view.hb?.usage?.memoryUsedBytes ?? 0n;
		if (Number(used) >= Number(c.ram) * FULL_RAM_FRAC) return false;
	}
	const need = candidateMemoryNeed(c, p);
	if (need > 0n && need > c.free) return false;
	return true;
}

export function candidateParts(c: Candidate, p?: Placement): CostPart[] {
	const out: CostPart[] = [
		create(CostPartSchema, {
			name: "cpu-busy",
			value: c.cpuBusy,
			detail: `${Math.round(c.cpuBusy * 100)}% of ${c.cpus} CPUs busy`,
		}),
		create(CostPartSchema, {
			name: "queued",
			value: c.queued / Math.max(c.cpus, 1),
			detail: `${c.queued} queued on ${c.cpus} CPUs`,
		}),
		create(CostPartSchema, {
			name: "cpu-pressure",
			value: (c.pressure / 100) * PRESSURE_COST,
			detail: `CPU pressure ${Math.round(c.pressure)}%`,
		}),
	];
	const add = (name: string, value: number, detail: string) => {
		out.push(create(CostPartSchema, { name, value, detail }));
	};
	if (candidateHolds(c, p)) {
		add("holding", HOLDING_COST, `it holds new work: ${c.admission?.reason ?? ""}`);
	}
	if (c.parked > 0) {
		add(
			"parked",
			HOLDING_COST + c.parked / Math.max(c.cpus, 1),
			`${c.parked} command(s) held beside a measurement`,
		);
	}
	const hs = hostStarvationCost(c.cpuBusy, c.hostUsage);
	if (hs.value > 0) {
		add("host-cpu-starvation", hs.value, hs.detail);
	}
	const memLow = p?.memoryLowBytes ?? 0n;
	const need = candidateMemoryNeed(c, p);
	if (need > c.free || (c.ram > 0n && c.free <= 0n)) {
		const avail = c.free > 0n ? c.free : 0n;
		const what =
			memLow > 0n
				? `the ${memLow} bytes guaranteed do not fit in the ${avail} free now`
				: need > 0n
					? `the ${need} bytes estimated do not fit in the ${avail} free now`
					: `no memory headroom free now (${avail} of ${c.ram} bytes)`;
		add("memory", FULL_COST, what);
	}
	if (c.asleep) {
		add("asleep", WAKE_COST, "it is asleep: the run waits for it to boot");
	} else if (!c.tunnelConnected) {
		add(
			"tunnel-reconnecting",
			RECONNECTING_COST,
			"its controller WebSocket tunnel is reconnecting",
		);
	}
	if (!c.mine) {
		add("move", MOVE_COST, "the session has no workspace here");
		if (!c.warm) {
			add("cold", COLD_COST, "no warm base of the project here");
		}
		if (c.broken) {
			add("broken", BROKEN_COST, "its newest base of the project failed");
		}
	}
	return out;
}

export function candidateCost(c: Candidate, p?: Placement): number {
	return candidateParts(c, p).reduce((sum, part) => sum + part.value, 0);
}

export function candidateUnfit(c: Candidate, p: Placement | undefined, hosts: string[]): string {
	if ((!c.view.online && !c.asleep) || !c.view.hb) {
		if (c.view.node.storageError) {
			return `${c.host} cannot open its storage: ${c.view.node.storageError}`;
		}
		return `${c.host} is offline`;
	}
	if (c.asleep && c.view.wakeFailed) {
		return `${c.host} did not wake when last woken`;
	}
	if (c.upgrading !== 0) {
		return `${c.host} is on build ${c.upgrading}, not yet the controller's`;
	}
	if (hosts.length > 0 && !hosts.includes(c.host)) {
		return `${c.host} keeps no bases of the project`;
	}
	const reqArch = p?.arch ?? "";
	const nodeArch = c.view.hb.resources?.arch ?? "";
	if (reqArch && nodeArch !== reqArch) {
		return `${c.host} is ${nodeArch}, not ${reqArch}`;
	}
	if (p?.bench && !(c.view.hb.resources?.benchCpus ?? "")) {
		return `${c.host} has no measurement CPUs`;
	}
	const reqRuntime = p?.runtime ?? "";
	if (reqRuntime && reqRuntime !== "vm" && reqRuntime !== "cladefs") {
		return `unknown runtime "${reqRuntime}" (want vm or cladefs)`;
	}
	if (p?.bench && reqRuntime === "vm") {
		return `${c.host} cannot run --bench inside vm runtime (requires cladefs)`;
	}
	const runtimes: string[] = (c.view.hb.resources?.runtimes ?? []).filter(
		(r) => r === "vm" || r === "cladefs",
	);
	if (runtimes.length === 0) {
		return `${c.host} has no available runtimes`;
	}
	const effRuntime = p?.bench ? "cladefs" : reqRuntime;
	if (effRuntime && !runtimes.includes(effRuntime)) {
		return `${c.host} does not support ${effRuntime} runtime`;
	}
	const memLow = p?.memoryLowBytes ?? 0n;
	if (memLow > c.ram) {
		return `${c.host} has ${c.ram} bytes of RAM, less than the ${memLow} guaranteed`;
	}
	const vmCpus = p?.vmCpus ?? 0;
	if (vmCpus > c.cpus) {
		return `${c.host} has ${c.cpus} CPUs, fewer than the ${vmCpus} its VM asks`;
	}
	const vmMem = p?.vmMemoryBytes ?? 0n;
	if (vmMem > c.ram) {
		return `${c.host} has ${c.ram} bytes of RAM, less than the ${vmMem} its VM asks`;
	}
	return "";
}

function fitsOnline(c: Candidate, p: Placement | undefined, hosts: string[]): boolean {
	const synth: Candidate = {
		...c,
		upgrading: 0,
		view: {
			...c.view,
			online: true,
			wakeFailed: false,
			hb: c.view.hb ?? create(HeartbeatRequestSchema, { resources: c.view.node.resources }),
		},
	};
	return candidateUnfit(synth, p, hosts) === "";
}

export function buildCandidates(
	nvs: NodeView[],
	project: string,
	mine: Map<string, string>,
	follows: (arch: string) => number,
	sharedWarm?: (arch: string, runtimes: string[]) => boolean,
): Candidate[] {
	const out: Candidate[] = [];
	for (const nv of nvs) {
		const res = nv.hb?.resources;
		const asleep = nv.node.asleep;
		const want = follows(res?.arch ?? "");
		const on = parseBuildNumber(res?.version ?? "");
		const upgrading = want !== 0 && on < want && !asleep ? on || -1 : 0;

		let warm = false;
		let newest: Base | undefined;
		for (const b of nv.bases) {
			if (b.project !== project) continue;
			if (b.state === BaseState.WARM) warm = true;
			if (!newest || b.generation > newest.generation) newest = b;
		}
		const nodeRuntimes = (res?.runtimes ?? []).filter((r) => r === "vm" || r === "cladefs");
		if (
			!warm &&
			sharedWarm &&
			nodeRuntimes.length > 0 &&
			sharedWarm(res?.arch ?? "", nodeRuntimes)
		) {
			warm = true;
		}
		const broken = !warm && !!newest && newest.state === BaseState.FAILED;

		let cpus = 0;
		let cpuBusy = 0;
		let pressure = 0;
		let admission: Admission | undefined;
		let ram = 0n;
		let free = 0n;
		let hostUsage: HostUsage | undefined;

		if (nv.hb) {
			cpus = nv.hb.resources?.cpus ?? 0;
			cpuBusy = nv.hb.usage?.cpuBusy ?? 0;
			pressure = nv.hb.usage?.cpuPressure ?? 0;
			admission = nv.hb.admission;
			ram = nv.hb.resources?.memoryBytes ?? 0n;
			const used = nv.hb.usage?.memoryUsedBytes ?? 0n;
			const reserved = nv.hb.reservedMemoryBytes;
			free = ram - (used > reserved ? used : reserved);
			if (asleep) {
				cpuBusy = 0;
				pressure = 0;
				admission = undefined;
				free = ram;
			} else if (nv.node.hostUsage) {
				hostUsage = nv.node.hostUsage;
				admission = mergeHostAdmission(admission, nv.hb.usage, hostUsage, 0);
			}
		}

		let queued = 0;
		let parked = 0;
		for (const cmd of nv.commands) {
			if (cmd.state === CommandState.QUEUED) queued++;
			else if (cmd.state === CommandState.PARKED) parked++;
		}

		out.push({
			view: nv,
			host: nv.node.hostname,
			mine: mine.get(nv.node.id) ?? "",
			warm,
			broken,
			asleep,
			tunnelConnected: nv.tunnelConnected !== false,
			upgrading,
			cpus,
			cpuBusy,
			queued,
			parked,
			ram,
			free,
			pressure,
			admission,
			hostUsage,
		});
	}
	return out;
}

export function rankCandidates(cs: Candidate[], p?: Placement): Candidate[] {
	return [...cs].sort((a, b) => {
		const ca = candidateCost(a, p);
		const cb = candidateCost(b, p);
		if (ca !== cb) return ca < cb ? -1 : 1;
		if (a.free !== b.free) return a.free > b.free ? -1 : 1;
		return a.host < b.host ? -1 : a.host > b.host ? 1 : 0;
	});
}

function upgradingHosts(scope: Candidate[], p: Placement | undefined, hosts: string[]): string[] {
	return scope
		.filter((c) => c.view.online && c.upgrading !== 0 && fitsOnline(c, p, hosts))
		.map((c) => c.host);
}

export function upgradingFor(
	cs: Candidate[],
	p: Placement | undefined,
	hosts: string[],
	syncs: boolean,
): string[] {
	let scope = cs;
	if (p?.node) {
		scope = cs.filter((c) => c.host === p.node);
	}
	const held = scope.filter((c) => c.mine !== "");
	if (!syncs && held.length > 0) {
		const away = held.filter(
			(c) => (!c.view.online || c.upgrading !== 0) && fitsOnline(c, p, hosts),
		);
		if (away.length > 0) scope = away;
	}
	return upgradingHosts(scope, p, hosts);
}

function refusal(
	scope: Candidate[],
	p: Placement | undefined,
	hosts: string[],
	stuck: boolean,
): HotbedError {
	const why = scope.map((c) => candidateUnfit(c, p, hosts));
	let code: HotbedError["code"] = ErrorCode.InvalidArgs;
	let retryable = false;
	let message = `no node can take the run: ${why.join("; ")}`;
	let suggestion =
		"hotbed nodes lists the nodes, their architectures and whether they are online; loosen --node or --arch";
	if (scope.length === 0) {
		message = "the cluster has no nodes";
	} else if (stuck) {
		message = `the run syncs no checkout, so it runs only in the session's workspace of the repo, and no node with one can take it: ${why.join("; ")}`;
		suggestion =
			"run it from a checkout of the repo, which it then syncs wherever it goes, or hotbed workspace release the workspace to start over elsewhere";
	}
	if (
		scope.length === 0 ||
		scope.some((c) => (!c.view.online || c.upgrading !== 0) && fitsOnline(c, p, hosts))
	) {
		code = ErrorCode.WorkerUnreachable;
		retryable = true;
	}
	if (!stuck && upgradingHosts(scope, p, hosts).length > 0) {
		suggestion = "retry once the node finishes upgrading";
	}
	return hotbedError(code, message, { suggestion, retryable });
}

export interface ChooseResult {
	chosen?: Candidate;
	/** Eligible second host to race workspace creation against when creating a new workspace. */
	runnerUp?: Candidate;
	/** True when runnerUp is close enough in cost to race from t=0 rather than hedging. */
	raceImmediate?: boolean;
	/** True when multiple fit nodes exist, waitForCapacity is set, and all are currently saturated. */
	allBusy?: boolean;
	busyHosts?: string[];
	why: string;
	err?: HotbedError;
}

function pickFromFit(
	fit: Candidate[],
	p: Placement | undefined,
	why: string,
	pinned: boolean,
	waitForCapacity: boolean,
): ChooseResult {
	const ranked = rankCandidates(fit, p);
	const withCap = ranked.filter((c) => candidateHasCapacity(c, p));
	if (withCap.length > 0) {
		const primary = withCap[0];
		let runnerUp: Candidate | undefined;
		let raceImmediate = false;
		if (!pinned && !primary.mine && !primary.asleep) {
			runnerUp = withCap.slice(1).find((c) => !c.mine && !c.asleep && c.tunnelConnected);
			if (runnerUp) {
				raceImmediate = candidateCost(runnerUp, p) - candidateCost(primary, p) <= RACE_COST_MARGIN;
			}
		}
		return { chosen: primary, runnerUp, raceImmediate, why };
	}
	if (!pinned && waitForCapacity) {
		return {
			allBusy: true,
			busyHosts: ranked.map((c) => c.host),
			why: "",
		};
	}
	return { chosen: ranked[0], why };
}

export function chooseCandidate(
	cs: Candidate[],
	p: Placement | undefined,
	hosts: string[],
	syncs: boolean,
	opts?: { waitForCapacity?: boolean },
): ChooseResult {
	const waitForCapacity = Boolean(opts?.waitForCapacity);
	let scope = cs;
	if (p?.node) {
		scope = cs.filter((c) => c.host === p.node);
		if (scope.length === 0) {
			return {
				why: "",
				err: hotbedError(ErrorCode.InvalidArgs, `no node is called ${p.node}`).withSuggestion(
					"hotbed nodes lists the nodes",
				),
			};
		}
	}
	const fits = (c: Candidate) => candidateUnfit(c, p, hosts) === "";
	const held = scope.filter((c) => c.mine !== "");
	if (!syncs && held.length > 0) {
		const fit = held.filter(fits);
		if (fit.length > 0) {
			return pickFromFit(
				fit,
				p,
				"the run syncs no checkout, so it stays in the session's workspace",
				Boolean(p?.node) || fit.length <= 1,
				waitForCapacity,
			);
		}
		const away = held.filter(
			(c) => (!c.view.online || c.upgrading !== 0) && fitsOnline(c, p, hosts),
		);
		if (away.length > 0) {
			return { why: "", err: refusal(away, p, hosts, true) };
		}
	}
	const fit = scope.filter(fits);
	if (fit.length === 0) {
		return { why: "", err: refusal(scope, p, hosts, false) };
	}
	return pickFromFit(
		fit,
		p,
		p?.node ? "the node asked for" : "the least costly node",
		Boolean(p?.node) || fit.length <= 1,
		waitForCapacity,
	);
}

export function buildPlacementDecision(
	cs: Candidate[],
	p: Placement | undefined,
	hosts: string[],
	chosen: Candidate | undefined,
	why: string,
	err: HotbedError | undefined,
	opts?: { noSync?: boolean; throwaway?: boolean },
): PlacementDecision {
	const d = create(PlacementDecisionSchema, {
		nodeId: err ? "" : (chosen?.view.node.id ?? ""),
		hostname: err ? "" : (chosen?.host ?? ""),
		reason: err ? err.message : why,
		candidates: [],
		request: p ? clone(PlacementSchema, p) : create(PlacementSchema),
		noSync: Boolean(opts?.noSync),
		throwaway: Boolean(opts?.throwaway),
	});
	for (const c of cs) {
		const unfit =
			p?.node && c.host !== p.node ? `the run asked for ${p.node}` : candidateUnfit(c, p, hosts);
		const pc = create(PlacementCandidateSchema, {
			nodeId: c.view.node.id,
			hostname: c.host,
			mine: c.mine !== "",
			chosen: !err && c.view.node.id === d.nodeId,
			unfit,
		});
		if (!unfit) {
			pc.parts = candidateParts(c, p);
			pc.cost = candidateCost(c, p);
		}
		d.candidates.push(pc);
	}
	return d;
}
