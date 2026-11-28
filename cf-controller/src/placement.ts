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
	type Node,
	type Placement,
	type PlacementDecision,
	type Resources,
	type Workspace,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, HotbedError, hotbedError } from "./errors.ts";
import { parseBuildNumber } from "./version.ts";

const MOVE_COST = 0.3;
const COLD_COST = 0.5;
const FULL_COST = 1;
const BROKEN_COST = 10;
const PRESSURE_COST = 1;
const HOLDING_COST = 1;
const RECONNECTING_COST = 5;
const SCARCE_ARCH_COST = 1.5;
export const FULL_RAM_FRAC = 0.95; // mirrors internal/worker/engine.go fullAt
export const RACE_COST_MARGIN = 0.35;

export interface ArchPreference {
	arch: string;
	cpus: number;
	archCpus: Map<string, number>;
}

/**
 * When a placement asks for no specific --arch and multiple architectures fit,
 * prefers the architecture with strictly more total online CPU capacity so
 * arch-agnostic runs default to the abundant pool instead of consuming scarce
 * architecture capacity (e.g. defaulting to arm64 over a single amd64 host).
 */
export function preferredArchFor(fit: Candidate[], p?: Placement): ArchPreference | undefined {
	if (p?.arch) return undefined;
	const archCpus = new Map<string, number>();
	for (const c of fit) {
		if (!c.tunnelConnected) continue;
		const arch = c.view.hb?.resources?.arch ?? "";
		if (!arch) continue;
		archCpus.set(arch, (archCpus.get(arch) ?? 0) + Math.max(c.cpus, 0));
	}
	if (archCpus.size < 2) return undefined;
	let bestArch = "";
	let bestCpus = -1;
	let tied = false;
	for (const [arch, cpus] of archCpus) {
		if (cpus > bestCpus) {
			bestArch = arch;
			bestCpus = cpus;
			tied = false;
		} else if (cpus === bestCpus) {
			tied = true;
		}
	}
	if (tied || bestCpus <= 0) return undefined;
	return { arch: bestArch, cpus: bestCpus, archCpus };
}

export interface NodeRow {
	node: Node;
	hb?: HeartbeatRequest;
}

export interface NodeView extends NodeRow {
	online: boolean;
	/** Whether the node currently holds an open /ws/worker reverse-RPC tunnel (defaults true when omitted). */
	tunnelConnected?: boolean;
	bases: Base[];
	workspaces: Workspace[];
	commands: Command[];
}

export interface Candidate {
	view: NodeView;
	host: string;
	mine: string;
	/** True when this session's workspace on this node already has a live command. */
	mineBusy?: boolean;
	warm: boolean;
	broken: boolean;
	tunnelConnected: boolean;
	upgrading: number;
	cpus: number;
	cpuBusy: number;
	running?: number;
	queued: number;
	parked: number;
	ram: bigint;
	free: bigint;
	pressure: number;
	admission?: Admission;
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
	if (!c.tunnelConnected) return false;
	if (c.mineBusy) return false;
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

export function candidateParts(c: Candidate, p?: Placement, archPref?: ArchPreference): CostPart[] {
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
	const running = c.running ?? 0;
	if (running > 0) {
		add("running", running / Math.max(c.cpus, 1), `${running} running on ${c.cpus} CPUs`);
	}
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
	if (!c.tunnelConnected) {
		add(
			"tunnel-reconnecting",
			RECONNECTING_COST,
			"its controller WebSocket tunnel is reconnecting",
		);
	}
	if (c.mineBusy) {
		add(
			"workspace-busy",
			HOLDING_COST,
			"the session's workspace here already has a live command running",
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
	const nodeArch = c.view.hb?.resources?.arch ?? "";
	if (archPref && nodeArch && nodeArch !== archPref.arch) {
		const myCpus = archPref.archCpus.get(nodeArch) ?? c.cpus;
		add(
			"arch-capacity",
			SCARCE_ARCH_COST,
			`${nodeArch} has less cluster capacity than ${archPref.arch} (${myCpus} vs ${archPref.cpus} CPUs)`,
		);
	}
	return out;
}

export function candidateCost(c: Candidate, p?: Placement, archPref?: ArchPreference): number {
	return candidateParts(c, p, archPref).reduce((sum, part) => sum + part.value, 0);
}

export function candidateUnfit(c: Candidate, p: Placement | undefined, hosts: string[]): string {
	if (!c.view.online || !c.view.hb) {
		return `${c.host} is offline`;
	}
	if (c.upgrading !== 0) {
		return `${c.host} is on build ${c.upgrading}, not yet the controller's`;
	}
	if (hosts.length > 0 && !hosts.includes(c.host)) {
		return `${c.host} keeps no bases of the project`;
	}
	// Each reason a node can't take the run ends with the requirement that
	// excludes it, so a caller can tell which to loosen.
	const reqArch = p?.arch ?? "";
	const nodeArch = c.view.hb.resources?.arch ?? "";
	if (reqArch && nodeArch !== reqArch) {
		return `${c.host} is ${nodeArch}, not ${reqArch} (--arch)`;
	}
	const benchCpus = cpuCount(c.view.hb.resources?.benchCpus ?? "");
	if (p?.bench && benchCpus === 0) {
		return `${c.host} has no measurement CPUs (--bench)`;
	}
	const reqRuntime = p?.runtime ?? "";
	if (reqRuntime && reqRuntime !== "vm" && reqRuntime !== "cladefs") {
		return `unknown runtime "${reqRuntime}" (want vm or cladefs) (--runtime)`;
	}
	const runtimes: string[] = (c.view.hb.resources?.runtimes ?? []).filter(
		(r) => r === "vm" || r === "cladefs",
	);
	if (runtimes.length === 0) {
		return `${c.host} has no available runtimes`;
	}
	// A measurement runs in either runtime: a VM's on the measurement CPUs
	// as a host command's does.
	if (reqRuntime && !runtimes.includes(reqRuntime)) {
		return `${c.host} offers the ${runtimes.join(" and ")} runtime, not ${reqRuntime} (--runtime)`;
	}
	const memLow = p?.memoryLowBytes ?? 0n;
	if (memLow > c.ram) {
		return `${c.host} has ${c.ram} bytes of RAM, less than the ${memLow} guaranteed (--mem-low)`;
	}
	const vmCpus = p?.vmCpus ?? 0;
	if (p?.bench && vmCpus > benchCpus) {
		return `${c.host} has ${benchCpus} measurement CPUs, fewer than the ${vmCpus} its VM asks, a vCPU on each (--bench --vm-cpus)`;
	}
	if (vmCpus > c.cpus) {
		return `${c.host} has ${c.cpus} CPUs, fewer than the ${vmCpus} its VM asks (--vm-cpus)`;
	}
	const vmMem = p?.vmMemoryBytes ?? 0n;
	if (vmMem > c.ram) {
		return `${c.host} has ${c.ram} bytes of RAM, less than the ${vmMem} its VM asks (--vm-mem)`;
	}
	return "";
}

/** cpuCount is how many CPUs a cpulist (4-7,12-15) names. */
export function cpuCount(list: string): number {
	let n = 0;
	for (const part of list.split(",")) {
		if (!part.trim()) continue;
		const [lo, hi] = part.split("-").map((x) => Number.parseInt(x, 10));
		n += hi === undefined ? 1 : hi - lo + 1;
	}
	return n;
}

function fitsOnline(c: Candidate, p: Placement | undefined, hosts: string[]): boolean {
	const synth: Candidate = {
		...c,
		upgrading: 0,
		view: {
			...c.view,
			online: true,
			hb: c.view.hb ?? create(HeartbeatRequestSchema, { resources: c.view.node.resources }),
		},
	};
	return candidateUnfit(synth, p, hosts) === "";
}

export function buildCandidates(
	nvs: NodeView[],
	project: string,
	mine: Map<string, string>,
	follows: (res: Resources | undefined) => number,
	sharedWarm?: (arch: string, runtimes: string[], os: string) => boolean,
): Candidate[] {
	const out: Candidate[] = [];
	for (const nv of nvs) {
		const res = nv.hb?.resources;
		const want = follows(res);
		const on = parseBuildNumber(res?.version ?? "");
		const upgrading = want !== 0 && on < want ? on || -1 : 0;

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
			sharedWarm(res?.arch ?? "", nodeRuntimes, res?.os ?? "")
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

		if (nv.hb) {
			cpus = nv.hb.resources?.cpus ?? 0;
			cpuBusy = nv.hb.usage?.cpuBusy ?? 0;
			pressure = nv.hb.usage?.cpuPressure ?? 0;
			admission = nv.hb.admission;
			ram = nv.hb.resources?.memoryBytes ?? 0n;
			const used = nv.hb.usage?.memoryUsedBytes ?? 0n;
			const reserved = nv.hb.reservedMemoryBytes;
			free = ram - (used > reserved ? used : reserved);
		}

		const mineId = mine.get(nv.node.id) ?? "";
		let mineBusy = false;
		let running = 0;
		let queued = 0;
		let parked = 0;
		for (const cmd of nv.commands) {
			if (cmd.state === CommandState.RUNNING) running++;
			else if (cmd.state === CommandState.QUEUED) queued++;
			else if (cmd.state === CommandState.PARKED) parked++;
			if (
				mineId &&
				cmd.workspaceId === mineId &&
				(cmd.state === CommandState.RUNNING ||
					cmd.state === CommandState.QUEUED ||
					cmd.state === CommandState.PARKED ||
					cmd.state === CommandState.PAUSED)
			) {
				mineBusy = true;
			}
		}

		out.push({
			view: nv,
			host: nv.node.hostname,
			mine: mineId,
			mineBusy,
			warm,
			broken,
			tunnelConnected: nv.tunnelConnected !== false,
			upgrading,
			cpus,
			cpuBusy,
			running,
			queued,
			parked,
			ram,
			free,
			pressure,
			admission,
		});
	}
	return out;
}

export function rankCandidates(
	cs: Candidate[],
	p?: Placement,
	archPref?: ArchPreference,
): Candidate[] {
	return [...cs].sort((a, b) => {
		const ca = candidateCost(a, p, archPref);
		const cb = candidateCost(b, p, archPref);
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
		"hotbed nodes lists the nodes, their architectures and whether they are online; loosen --arch or resource requirements";
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
	archPref?: ArchPreference,
): ChooseResult {
	const ranked = rankCandidates(fit, p, archPref);
	const withCap = ranked.filter((c) => candidateHasCapacity(c, p));
	if (withCap.length > 0) {
		const primary = withCap[0];
		let runnerUp: Candidate | undefined;
		let raceImmediate = false;
		if (!pinned && !primary.mine) {
			runnerUp = withCap.slice(1).find((c) => !c.mine && c.tunnelConnected);
			if (runnerUp) {
				raceImmediate =
					candidateCost(runnerUp, p, archPref) - candidateCost(primary, p, archPref) <=
					RACE_COST_MARGIN;
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
	if (p?.node) {
		return {
			why: "",
			err: hotbedError(
				ErrorCode.InvalidArgs,
				`placement by --node (${p.node}) is no longer supported; specify requirements instead (--arch, --runtime, --bench, --mem-low, --vm-cpus, --vm-mem)`,
			).withSuggestion(
				"drop --node, pass requirements such as --arch amd64|arm64, and run `hotbed upgrade`",
			),
		};
	}
	const waitForCapacity = Boolean(opts?.waitForCapacity);
	const scope = cs;
	const fits = (c: Candidate) => candidateUnfit(c, p, hosts) === "";
	const allFit = scope.filter(fits);
	const archPref = preferredArchFor(allFit, p);
	const held = scope.filter((c) => c.mine !== "");
	if (!syncs && held.length > 0) {
		const fit = held.filter(fits);
		if (fit.length > 0) {
			return pickFromFit(
				fit,
				p,
				"the run syncs no checkout, so it stays in the session's workspace",
				fit.length <= 1,
				waitForCapacity,
				archPref,
			);
		}
		const away = held.filter(
			(c) => (!c.view.online || c.upgrading !== 0) && fitsOnline(c, p, hosts),
		);
		if (away.length > 0) {
			return { why: "", err: refusal(away, p, hosts, true) };
		}
	}
	if (allFit.length === 0) {
		return { why: "", err: refusal(scope, p, hosts, false) };
	}
	return pickFromFit(
		allFit,
		p,
		"the least costly node",
		allFit.length <= 1,
		waitForCapacity,
		archPref,
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
	const archPref = preferredArchFor(
		cs.filter((c) => candidateUnfit(c, p, hosts) === ""),
		p,
	);
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
		const unfit = candidateUnfit(c, p, hosts);
		const pc = create(PlacementCandidateSchema, {
			nodeId: c.view.node.id,
			hostname: c.host,
			mine: c.mine !== "",
			chosen: !err && c.view.node.id === d.nodeId,
			unfit,
		});
		if (!unfit) {
			pc.parts = candidateParts(c, p, archPref);
			pc.cost = candidateCost(c, p, archPref);
		}
		d.candidates.push(pc);
	}
	return d;
}
