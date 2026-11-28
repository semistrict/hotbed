import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { HeartbeatRequestSchema, ResourcesSchema } from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { reportsHeldBases } from "../src/services/cluster.ts";

function heartbeat(version: string, runtimes: string[]) {
	return create(HeartbeatRequestSchema, {
		resources: create(ResourcesSchema, { arch: "arm64", version, runtimes }),
	});
}

// A heartbeat says which bases its worker holds only when the worker could
// tell: its cladefs backend up, on the controller's build. Any other leaves
// the holds it last reported, so no base is deleted under a worker that holds
// it but said nothing.
describe("reportsHeldBases", () => {
	it("trusts a worker on the controller's build with its cladefs runtime up", () => {
		expect(reportsHeldBases(heartbeat("build 300 (0123456789ab)", ["vm"]), 300)).toBe(true);
		expect(reportsHeldBases(heartbeat("build 301 (0123456789ab)", ["cladefs"]), 300)).toBe(true);
	});

	it("trusts any worker while the controller follows no release", () => {
		expect(reportsHeldBases(heartbeat("dev (0123456789ab)", ["vm"]), 0)).toBe(true);
	});

	it("leaves the holds of a worker on an older build, which may not know held_bases", () => {
		expect(reportsHeldBases(heartbeat("build 299 (0123456789ab)", ["vm"]), 300)).toBe(false);
		expect(reportsHeldBases(heartbeat("dev (0123456789ab)", ["vm"]), 300)).toBe(false);
	});

	it("leaves the holds of a worker whose cladefs runtime is down", () => {
		expect(reportsHeldBases(heartbeat("build 300 (0123456789ab)", []), 300)).toBe(false);
	});
});
