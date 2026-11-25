import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { PlacementSchema, ResourcesSchema } from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { type Candidate, candidateUnfit } from "../src/placement.ts";

// A vm-runtime node with 8 CPUs and 16 GiB of RAM, online and up to date.
function vmNode(): Candidate {
	const resources = create(ResourcesSchema, {
		arch: "arm64",
		cpus: 8,
		memoryBytes: 16n << 30n,
		runtimes: ["vm"],
	});
	return {
		view: { online: true, hb: { resources }, node: {} },
		host: "big",
		mine: "",
		warm: true,
		broken: false,
		asleep: false,
		tunnelConnected: true,
		upgrading: 0,
		cpus: 8,
		cpuBusy: 0,
		queued: 0,
		parked: 0,
		ram: 16n << 30n,
		free: 16n << 30n,
		pressure: 0,
	} as unknown as Candidate;
}

describe("candidateUnfit", () => {
	it("leaves out a node too small for the VM a run asks", () => {
		const fits = create(PlacementSchema, { runtime: "vm", vmCpus: 8, vmMemoryBytes: 16n << 30n });
		expect(candidateUnfit(vmNode(), fits, [])).toBe("");
		const cpus = create(PlacementSchema, { runtime: "vm", vmCpus: 9 });
		expect(candidateUnfit(vmNode(), cpus, [])).toBe("big has 8 CPUs, fewer than the 9 its VM asks");
		const mem = create(PlacementSchema, { runtime: "vm", vmMemoryBytes: 17n << 30n });
		expect(candidateUnfit(vmNode(), mem, [])).toBe(
			"big has 17179869184 bytes of RAM, less than the 18253611008 its VM asks",
		);
	});
});
