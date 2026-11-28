import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import {
	AdmissionSchema,
	AdmissionSignalSchema,
	CommandSchema,
	CommandState,
	HeartbeatRequestSchema,
	NodeSchema,
	PlacementSchema,
	ResourcesSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import {
	type Candidate,
	buildCandidates,
	buildPlacementDecision,
	candidateCost,
	candidateHasCapacity,
	candidateUnfit,
	chooseCandidate,
	cpuCount,
} from "../src/placement.ts";

// A vm-runtime node with 8 CPUs and 16 GiB of RAM, online and up to date.
function vmNode(overrides: Partial<Candidate> = {}): Candidate {
	const resources = create(ResourcesSchema, {
		arch: "arm64",
		cpus: 8,
		memoryBytes: 16n << 30n,
		runtimes: ["vm"],
	});
	return {
		view: { online: true, hb: { resources }, node: { id: overrides.host ?? "big" } },
		host: "big",
		mine: "",
		warm: true,
		broken: false,
		tunnelConnected: true,
		upgrading: 0,
		cpus: 8,
		cpuBusy: 0,
		queued: 0,
		parked: 0,
		ram: 16n << 30n,
		free: 16n << 30n,
		pressure: 0,
		...overrides,
	} as unknown as Candidate;
}

// A vm-runtime node of arch with 16 CPUs, measurement CPUs benchCpus ("" for
// none).
function benchNode(host: string, arch: string, benchCpus: string): Candidate {
	const resources = create(ResourcesSchema, {
		arch,
		cpus: 16,
		memoryBytes: 16n << 30n,
		benchCpus,
		runtimes: ["vm"],
	});
	return vmNode({
		host,
		cpus: 16,
		view: { online: true, hb: { resources }, node: { id: host } },
	} as unknown as Partial<Candidate>);
}

describe("cpuCount", () => {
	it("counts a cpulist's CPUs", () => {
		expect(cpuCount("")).toBe(0);
		expect(cpuCount("7-10")).toBe(4);
		expect(cpuCount("4-7,12-15")).toBe(8);
		expect(cpuCount("3")).toBe(1);
	});
});

describe("candidateUnfit", () => {
	it("leaves out a node too small for the VM a run asks", () => {
		const fits = create(PlacementSchema, { runtime: "vm", vmCpus: 8, vmMemoryBytes: 16n << 30n });
		expect(candidateUnfit(vmNode(), fits, [])).toBe("");
		const cpus = create(PlacementSchema, { runtime: "vm", vmCpus: 9 });
		expect(candidateUnfit(vmNode(), cpus, [])).toBe(
			"big has 8 CPUs, fewer than the 9 its VM asks (--vm-cpus)",
		);
		const mem = create(PlacementSchema, { runtime: "vm", vmMemoryBytes: 17n << 30n });
		expect(candidateUnfit(vmNode(), mem, [])).toBe(
			"big has 17179869184 bytes of RAM, less than the 18253611008 its VM asks (--vm-mem)",
		);
	});
	it("places a measurement on a vm node with measurement CPUs, naming what excludes the rest", () => {
		const bench = create(PlacementSchema, { arch: "amd64", bench: true });
		const ubu1 = benchNode("ubu1", "amd64", "4-7,12-15");
		const amd = benchNode("plain", "amd64", "");
		const mac = benchNode("asahimbp", "arm64", "7-10");
		expect(candidateUnfit(ubu1, bench, [])).toBe("");
		expect(candidateUnfit(amd, bench, [])).toBe("plain has no measurement CPUs (--bench)");
		expect(candidateUnfit(mac, bench, [])).toBe("asahimbp is arm64, not amd64 (--arch)");
		expect(chooseCandidate([amd, mac, ubu1], bench, [], true).chosen?.host).toBe("ubu1");
		expect(candidateUnfit(ubu1, create(PlacementSchema, { bench: true, runtime: "vm" }), [])).toBe(
			"",
		);
		expect(
			candidateUnfit(ubu1, create(PlacementSchema, { bench: true, runtime: "cladefs" }), []),
		).toBe("ubu1 offers the vm runtime, not cladefs (--runtime)");
		expect(candidateUnfit(ubu1, create(PlacementSchema, { bench: true, vmCpus: 8 }), [])).toBe("");
		expect(candidateUnfit(ubu1, create(PlacementSchema, { bench: true, vmCpus: 9 }), [])).toBe(
			"ubu1 has 8 measurement CPUs, fewer than the 9 its VM asks, a vCPU on each (--bench --vm-cpus)",
		);
	});
	it("rejects deprecated Placement.node from older CLIs instead of silently ignoring it", () => {
		const a = vmNode({ host: "ubu1" });
		const res = chooseCandidate([a], create(PlacementSchema, { node: "ubu1" }), [], true);
		expect(res.err).toBeDefined();
		expect(res.err?.code).toBe("INVALID_ARGS");
		expect(res.err?.message).toContain("--arch");
		expect(res.err?.suggestion).toContain("hotbed upgrade");
	});
});

describe("buildPlacementDecision", () => {
	it("records the requested placement constraints, noSync, and throwaway", () => {
		const node = vmNode();
		const req = create(PlacementSchema, { arch: "arm64", memoryLowBytes: 4n << 30n });
		const d = buildPlacementDecision([node], req, [], node, "the least costly node", undefined, {
			noSync: true,
			throwaway: true,
		});
		expect(d.request?.arch).toBe("arm64");
		expect(d.request?.memoryLowBytes).toBe(4n << 30n);
		expect(d.noSync).toBe(true);
		expect(d.throwaway).toBe(true);
		expect(d.hostname).toBe("big");
	});
});

describe("late-binding capacity and 2-host racing", () => {
	const holding = create(AdmissionSchema, {
		holding: true,
		reason: "CPU pressure 75%",
		signals: [create(AdmissionSignalSchema, { name: "cpu-pressure", holding: true })],
	});

	it("returns allBusy instead of committing to one full node when waitForCapacity is true", () => {
		const a = vmNode({ host: "a", admission: holding });
		const b = vmNode({ host: "b", free: 0n });
		expect(candidateHasCapacity(a)).toBe(false);
		expect(candidateHasCapacity(b)).toBe(false);

		const res = chooseCandidate([a, b], undefined, [], true, { waitForCapacity: true });
		expect(res.allBusy).toBe(true);
		expect(res.chosen).toBeUndefined();
		expect(res.busyHosts).toEqual(["a", "b"]);

		// When only one host fits, choose it immediately so worker queues it.
		const single = chooseCandidate([a], undefined, [], true, {
			waitForCapacity: true,
		});
		expect(single.allBusy).toBeUndefined();
		expect(single.chosen?.host).toBe("a");
	});

	it("prefers an eligible node with capacity over a holding/full warm node", () => {
		const fullMine = vmNode({ host: "ubu1", mine: "ws1", admission: holding });
		const freeCold = vmNode({ host: "asahimbp", warm: false, cpuBusy: 0.4 });
		const res = chooseCandidate([fullMine, freeCold], undefined, [], true, {
			waitForCapacity: true,
		});
		expect(res.chosen?.host).toBe("asahimbp");
	});

	it("races immediately when top 2 nodes are within cost margin and hedges otherwise", () => {
		const a = vmNode({ host: "a", cpuBusy: 0.1 });
		const b = vmNode({ host: "b", cpuBusy: 0.2 });
		const close = chooseCandidate([a, b], undefined, [], true, { waitForCapacity: true });
		expect(close.chosen?.host).toBe("a");
		expect(close.runnerUp?.host).toBe("b");
		expect(close.raceImmediate).toBe(true);

		const busy = vmNode({ host: "b", cpuBusy: 0.9 });
		const far = chooseCandidate([a, busy], undefined, [], true, { waitForCapacity: true });
		expect(far.chosen?.host).toBe("a");
		expect(far.runnerUp?.host).toBe("b");
		expect(far.raceImmediate).toBe(false);
	});

	it("prefers an idle smaller-RAM node over a bigger node already running a command", () => {
		const mbp = vmNode({
			host: "ramons-macbook-pro",
			cpus: 15,
			ram: 48n << 30n,
			free: 16n << 30n,
			running: 1,
		});
		const mac = vmNode({
			host: "mac",
			cpus: 11,
			ram: 18n << 30n,
			free: 9n << 30n,
			running: 0,
		});
		expect(candidateCost(mbp)).toBeGreaterThan(candidateCost(mac));
		const res = chooseCandidate([mbp, mac], undefined, [], true, { waitForCapacity: true });
		expect(res.chosen?.host).toBe("mac");
		expect(res.runnerUp?.host).toBe("ramons-macbook-pro");
	});

	it("counts running commands and passes os to sharedWarm in buildCandidates", () => {
		const seenOS: string[] = [];
		const cs = buildCandidates(
			[
				{
					node: create(NodeSchema, { id: "n-mac", hostname: "mac", online: true }),
					online: true,
					hb: create(HeartbeatRequestSchema, {
						resources: create(ResourcesSchema, {
							os: "darwin",
							arch: "arm64",
							cpus: 11,
							memoryBytes: 18n << 30n,
							runtimes: ["vm"],
						}),
					}),
					bases: [],
					workspaces: [],
					commands: [create(CommandSchema, { id: "c1", state: CommandState.RUNNING })],
				},
			],
			"cladefs",
			new Map(),
			() => 0,
			(_arch, _runtimes, os) => {
				seenOS.push(os);
				return os !== "darwin";
			},
		);
		expect(seenOS).toEqual(["darwin"]);
		expect(cs[0].warm).toBe(false);
		expect(cs[0].running).toBe(1);
	});
});
