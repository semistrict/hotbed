import { clone, create } from "@bufbuild/protobuf";
import {
	AdmissionSchema,
	AdmissionSignalSchema,
	HostUsageSchema,
	SamplesSchema,
	SampleSchema,
	type Admission,
	type AdmissionSignal,
	type HostUsage,
	type NodeUsage,
	type Samples,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

export const HOST_USAGE_TTL_MS = 45_000;
const MAX_HOST_CLOCK_SKEW_MS = 10_000;
const LIVE_WINDOW_MS = 15 * 60_000;

const HOST_MEM_PRESSURE_HOLD_AT = 50.0;
const HOST_CPU_BUSY_HOLD_AT = 0.85;
const HOST_LOAD_FACTOR_HOLD_AT = 1.25;
const HOST_STEAL_GUEST_MIN = 0.5;
const HOST_STEAL_DIFF_HOLD_AT = 0.35;

const HOST_STEAL_COST_GUEST_MIN = 0.3;
const HOST_STEAL_COST_DIFF_MIN = 0.1;
const HOST_BUSY_COST_START = 0.7;
const HOST_LOAD_COST_START = 1.0;

interface HostUsageEntry {
	usage: HostUsage;
	receivedAt: number;
	expiresAt: number;
}

export interface HostUsagePoint {
	at: number;
	expiresAt: number;
	vmCPUBusy: number;
	hostCPUBusy: number;
}

export class HostUsageState {
	private readonly latest = new Map<string, HostUsageEntry>();
	private readonly history = new Map<string, HostUsagePoint[]>();

	prune(nowMs: number): void {
		for (const [host, e] of this.latest) {
			if (nowMs >= e.expiresAt) {
				this.latest.delete(host);
			}
		}
		const cutoff = nowMs - LIVE_WINDOW_MS;
		for (const [host, pts] of this.history) {
			let keep = 0;
			while (keep < pts.length && pts[keep].expiresAt <= cutoff) keep++;
			if (keep >= pts.length) this.history.delete(host);
			else if (keep > 0) this.history.set(host, pts.slice(keep));
		}
	}

	record(
		host: string,
		u: HostUsage,
		effectiveAt: number,
		expiresAt: number,
		nowMs: number,
	): boolean {
		this.prune(nowMs);
		const prev = this.latest.get(host);
		if (prev) {
			if (u.sampledAt < prev.usage.sampledAt) return false;
			if (u.sampledAt === prev.usage.sampledAt && expiresAt > prev.expiresAt) {
				expiresAt = prev.expiresAt;
			}
		}
		if (nowMs >= expiresAt) return false;
		this.latest.set(host, { usage: u, receivedAt: nowMs, expiresAt });
		const pts = this.history.get(host) ?? [];
		const pt: HostUsagePoint = {
			at: effectiveAt,
			expiresAt,
			vmCPUBusy: u.vmCpuBusy,
			hostCPUBusy: u.hostCpuBusy,
		};
		if (pts.length > 0) {
			const last = pts[pts.length - 1];
			if (pt.at < last.at) return true;
			if (pt.at === last.at) pts[pts.length - 1] = pt;
			else pts.push(pt);
		} else {
			pts.push(pt);
		}
		const cutoff = nowMs - LIVE_WINDOW_MS;
		let keep = 0;
		while (keep < pts.length && pts[keep].expiresAt <= cutoff) keep++;
		if (keep >= pts.length) this.history.delete(host);
		else this.history.set(host, keep > 0 ? pts.slice(keep) : pts);
		return true;
	}

	get(host: string, nowMs: number): HostUsage | undefined {
		this.prune(nowMs);
		const e = this.latest.get(host);
		if (!e || nowMs >= e.expiresAt) return undefined;
		const cp = clone(HostUsageSchema, e.usage);
		cp.remainingTtlMs = BigInt(Math.max(1, e.expiresAt - nowMs));
		return cp;
	}

	snapshotHistory(host: string, nowMs: number): HostUsagePoint[] {
		this.prune(nowMs);
		return [...(this.history.get(host) ?? [])];
	}
}

function clampFinite(v: number, lo: number, hi: number): number {
	if (!Number.isFinite(v)) return lo;
	if (v < lo) return lo;
	if (v > hi) return hi;
	return v;
}

export function prepareHostUsage(
	input: HostUsage,
	nowMs: number,
): { usage: HostUsage; effectiveAt: number; expiresAt: number } {
	const sampledAtNum = Number(input.sampledAt);
	if (sampledAtNum < 0) {
		throw hotbedError(ErrorCode.InvalidArgs, `host usage sampled_at ${sampledAtNum} is negative`);
	}
	let sampledAt = nowMs;
	if (sampledAtNum > 0) {
		sampledAt = sampledAtNum;
		if (sampledAt > nowMs + MAX_HOST_CLOCK_SKEW_MS) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`host usage sampled_at is ${sampledAt - nowMs}ms in the future (max skew ${MAX_HOST_CLOCK_SKEW_MS}ms)`,
			);
		}
		if (nowMs >= sampledAt + HOST_USAGE_TTL_MS) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`host usage sample is expired (age ${nowMs - sampledAt}ms >= ttl ${HOST_USAGE_TTL_MS}ms)`,
			);
		}
	}
	const effectiveAt = Math.min(sampledAt, nowMs);
	const expiresAt = effectiveAt + HOST_USAGE_TTL_MS;
	const usage = create(HostUsageSchema, {
		vmCpuBusy: clampFinite(input.vmCpuBusy, 0, 1),
		hostCpuBusy: clampFinite(input.hostCpuBusy, 0, 1),
		hostLoad1m: clampFinite(input.hostLoad1m, 0, Number.MAX_VALUE),
		hostCpus: Math.max(0, input.hostCpus),
		hostMemoryPressure: clampFinite(input.hostMemoryPressure, 0, 100),
		sampledAt: BigInt(sampledAtNum > 0 ? sampledAtNum : nowMs),
	});
	return { usage, effectiveAt, expiresAt };
}

export function hostStarvation(
	guestUsage: NodeUsage | undefined,
	hu: HostUsage | undefined,
): { holding: boolean; reason: string; signals: AdmissionSignal[] } {
	if (!hu) return { holding: false, reason: "", signals: [] };
	const guestBusy = clampFinite(guestUsage?.cpuBusy ?? 0, 0, 1);
	const vmBusy = clampFinite(hu.vmCpuBusy, 0, 1);
	const hostBusy = clampFinite(hu.hostCpuBusy, 0, 1);
	const load1m = clampFinite(hu.hostLoad1m, 0, Number.MAX_VALUE);
	const hostCPUs = hu.hostCpus;
	const memPressure = clampFinite(hu.hostMemoryPressure, 0, 100);

	const steal = Math.max(0, guestBusy - vmBusy);
	const highHostCPU = hostBusy >= HOST_CPU_BUSY_HOLD_AT;
	const highHostLoad = hostCPUs > 0 && load1m >= hostCPUs * HOST_LOAD_FACTOR_HOLD_AT;
	const cpuSteal = guestBusy >= HOST_STEAL_GUEST_MIN && steal >= HOST_STEAL_DIFF_HOLD_AT;
	const cpuHolding = highHostCPU || highHostLoad || cpuSteal;

	const cpuSig = create(AdmissionSignalSchema, {
		name: "host-cpu-starvation",
		unit: "%",
		holding: cpuHolding,
	});
	let cpuReason = "";
	if (cpuSteal) {
		cpuSig.value = steal * 100;
		cpuSig.limit = HOST_STEAL_DIFF_HOLD_AT * 100;
		cpuSig.detail = `guest ${Math.round(guestBusy * 100)}% vs VM ${Math.round(vmBusy * 100)}% on host (${Math.round(steal * 100)}% stolen); host ${Math.round(hostBusy * 100)}% busy, load ${load1m.toFixed(1)}/${hostCPUs}`;
		cpuReason = `host CPU starvation: guest ${Math.round(guestBusy * 100)}% vs VM ${Math.round(vmBusy * 100)}% on host (${Math.round(steal * 100)}% stolen)`;
	} else if (highHostCPU) {
		cpuSig.value = hostBusy * 100;
		cpuSig.limit = HOST_CPU_BUSY_HOLD_AT * 100;
		cpuSig.detail = `host ${Math.round(hostBusy * 100)}% busy, load ${load1m.toFixed(1)}/${hostCPUs} (VM ${Math.round(vmBusy * 100)}%)`;
		cpuReason = `host CPU busy ${Math.round(hostBusy * 100)}% (limit ${Math.round(HOST_CPU_BUSY_HOLD_AT * 100)}%)`;
	} else if (highHostLoad) {
		cpuSig.value = (load1m / hostCPUs) * 100;
		cpuSig.limit = HOST_LOAD_FACTOR_HOLD_AT * 100;
		cpuSig.detail = `host load ${load1m.toFixed(1)}/${hostCPUs} (host ${Math.round(hostBusy * 100)}% busy, VM ${Math.round(vmBusy * 100)}%)`;
		cpuReason = `host load ${load1m.toFixed(1)} on ${hostCPUs} CPUs (limit ${(hostCPUs * HOST_LOAD_FACTOR_HOLD_AT).toFixed(1)})`;
	} else {
		cpuSig.value = hostBusy * 100;
		cpuSig.limit = HOST_CPU_BUSY_HOLD_AT * 100;
		cpuSig.detail =
			hostCPUs > 0
				? `VM ${Math.round(vmBusy * 100)}% on host; host ${Math.round(hostBusy * 100)}% busy, load ${load1m.toFixed(1)}/${hostCPUs}`
				: `VM ${Math.round(vmBusy * 100)}% on host; host ${Math.round(hostBusy * 100)}% busy`;
	}

	const memHolding = memPressure >= HOST_MEM_PRESSURE_HOLD_AT;
	let memDetail = "macOS memory pressure normal";
	if (memPressure >= 100) memDetail = "macOS memory pressure critical";
	else if (memPressure >= HOST_MEM_PRESSURE_HOLD_AT) memDetail = "macOS memory pressure warn";

	const memSig = create(AdmissionSignalSchema, {
		name: "host-memory-pressure",
		value: memPressure,
		limit: HOST_MEM_PRESSURE_HOLD_AT,
		unit: "%",
		holding: memHolding,
		detail: memDetail,
	});

	const holding = cpuHolding || memHolding;
	let reason = "";
	if (cpuHolding && memHolding) {
		reason = `${cpuReason}; host memory pressure ${Math.round(memPressure)}%`;
	} else if (cpuHolding) {
		reason = cpuReason;
	} else if (memHolding) {
		reason = `host memory pressure ${Math.round(memPressure)}% (limit ${Math.round(HOST_MEM_PRESSURE_HOLD_AT)}%)`;
	}
	return { holding, reason, signals: [cpuSig, memSig] };
}

export function mergeHostAdmission(
	existing: Admission | undefined,
	guestUsage: NodeUsage | undefined,
	hu: HostUsage | undefined,
	nowMs: number,
): Admission | undefined {
	if (!hu) return existing;
	const { holding, reason, signals: hostSignals } = hostStarvation(guestUsage, hu);
	if (!existing && !holding) return undefined;
	const out = existing ? clone(AdmissionSchema, existing) : create(AdmissionSchema);
	for (const hs of hostSignals) {
		const idx = out.signals.findIndex((s) => s.name === hs.name);
		if (idx < 0) {
			out.signals.push(hs);
		} else if (hs.holding && !out.signals[idx].holding) {
			hs.waiting = out.signals[idx].waiting;
			out.signals[idx] = hs;
		}
	}
	if (holding) {
		out.holding = true;
		if (!out.reason) out.reason = reason;
		if (out.since === 0n) {
			out.since = hu.sampledAt > 0n ? hu.sampledAt : BigInt(nowMs);
		}
	}
	return out;
}

export function hostStarvationCost(
	guestBusy: number,
	hu: HostUsage | undefined,
): { value: number; detail: string } {
	if (!hu) return { value: 0, detail: "" };
	const gb = clampFinite(guestBusy, 0, 1);
	const vmBusy = clampFinite(hu.vmCpuBusy, 0, 1);
	const hostBusy = clampFinite(hu.hostCpuBusy, 0, 1);
	const load1m = clampFinite(hu.hostLoad1m, 0, Number.MAX_VALUE);
	const hostCPUs = hu.hostCpus;

	let stealPenalty = 0;
	let hostPenalty = 0;
	let loadPenalty = 0;
	const steal = Math.max(0, gb - vmBusy);
	if (gb >= HOST_STEAL_COST_GUEST_MIN && steal > HOST_STEAL_COST_DIFF_MIN) {
		stealPenalty = steal;
	}
	if (hostBusy > HOST_BUSY_COST_START) {
		hostPenalty = Math.min(1, (hostBusy - HOST_BUSY_COST_START) / (1 - HOST_BUSY_COST_START));
	}
	if (hostCPUs > 0) {
		const loadFrac = load1m / hostCPUs;
		if (loadFrac > HOST_LOAD_COST_START) {
			loadPenalty = Math.min(1, (loadFrac - HOST_LOAD_COST_START) / 0.5);
		}
	}
	const value = Math.max(stealPenalty, hostPenalty, loadPenalty);
	if (value <= 0) return { value: 0, detail: "" };
	const detail =
		hostCPUs > 0
			? `VM ${Math.round(vmBusy * 100)}% vs guest ${Math.round(gb * 100)}%, host ${Math.round(hostBusy * 100)}% busy, load ${load1m.toFixed(1)}/${hostCPUs}`
			: `VM ${Math.round(vmBusy * 100)}% vs guest ${Math.round(gb * 100)}%, host ${Math.round(hostBusy * 100)}% busy`;
	return { value, detail };
}

export function overlayHostSamples(
	ser: Record<string, Samples>,
	nodes: Array<{ id: string; hostname: string }>,
	state: HostUsageState,
	nowMs: number,
): Record<string, Samples> {
	const out: Record<string, Samples> = { ...ser };
	for (const n of nodes) {
		const src = ser[n.id];
		if (!src || src.samples.length === 0) continue;
		const pts = state.snapshotHistory(n.hostname, nowMs);
		if (pts.length === 0) continue;
		const cloned = create(SamplesSchema, { samples: [] });
		let j = 0;
		for (const sp of src.samples) {
			const cp = clone(SampleSchema, sp);
			const t = Number(cp.t);
			while (j + 1 < pts.length && pts[j + 1].at <= t) j++;
			if (pts[j].at <= t && t < pts[j].expiresAt) {
				cp.vmCpuBusy = pts[j].vmCPUBusy;
				cp.hostCpuBusy = pts[j].hostCPUBusy;
			}
			cloned.samples.push(cp);
		}
		out[n.id] = cloned;
	}
	return out;
}
