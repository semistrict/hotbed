import { env, runInDurableObject } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vite-plus/test";
import type { ControllerEnv } from "../src/controller-do.ts";
import {
	CladefsCellSchema,
	HeartbeatRequestSchema,
	ResourcesSchema,
	SharedBaseSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { reportsHeldBases } from "../src/services/cluster.ts";
import type { ControllerStore } from "../src/store/index.ts";

function heartbeat(version: string, runtimes: string[], os = "linux") {
	return create(HeartbeatRequestSchema, {
		resources: create(ResourcesSchema, { os, arch: "arm64", version, runtimes }),
	});
}

// A heartbeat says which bases its worker holds only when the worker could
// tell: its cladefs backend up, sharing the R2 bucket (not darwin/machined),
// on the controller's build. Any other leaves the holds it last reported, so
// no base is deleted under a worker that holds it but said nothing.
describe("reportsHeldBases", () => {
	it("trusts a worker on the controller's build with its cladefs runtime up", () => {
		expect(reportsHeldBases(heartbeat("build 300 (0123456789ab)", ["vm"]), 300)).toBe(true);
		expect(reportsHeldBases(heartbeat("build 301 (0123456789ab)", ["cladefs"]), 300)).toBe(true);
	});

	it("trusts any linux worker while the controller follows no release", () => {
		expect(reportsHeldBases(heartbeat("dev (0123456789ab)", ["vm"]), 0)).toBe(true);
	});

	it("rejects darwin/machined workers whose cells are strictly local-only", () => {
		expect(reportsHeldBases(heartbeat("build 300 (0123456789ab)", ["vm"], "darwin"), 300)).toBe(
			false,
		);
		expect(reportsHeldBases(heartbeat("dev (0123456789ab)", ["vm"], "darwin"), 0)).toBe(false);
	});

	it("leaves the holds of a worker on an older build, which may not know held_bases", () => {
		expect(reportsHeldBases(heartbeat("build 299 (0123456789ab)", ["vm"]), 300)).toBe(false);
		expect(reportsHeldBases(heartbeat("dev (0123456789ab)", ["vm"]), 300)).toBe(false);
	});

	it("leaves the holds of a worker whose cladefs runtime is down", () => {
		expect(reportsHeldBases(heartbeat("build 300 (0123456789ab)", []), 300)).toBe(false);
	});
});

describe("CladefsStore.purgeNodeSharedBases", () => {
	it("removes leaked local-only shared_bases rows and restores the previous R2 base", async () => {
		const stub = (env as unknown as ControllerEnv).CONTROLLER.getByName("purge-shared-bases");
		await runInDurableObject(stub, (instance) => {
			const store = (instance as unknown as { store: ControllerStore }).store;
			const linuxBase = create(SharedBaseSchema, {
				cell: create(CladefsCellSchema, {
					tenant: 0n,
					cell: 1001n,
					version: 1n,
					project: "cladefs",
					arch: "arm64",
					runtime: "vm",
					baseId: "cladefs-vm-linux",
				}),
			});
			const darwinLeak = create(SharedBaseSchema, {
				cell: create(CladefsCellSchema, {
					tenant: 0n,
					cell: 2002n,
					version: 2n,
					project: "cladefs",
					arch: "arm64",
					runtime: "vm",
					baseId: "cladefs-vm-mac-local",
				}),
			});
			store.cladefs.recordHeldBases("linux-node", "arm64", [linuxBase], [], 1_000, 0n);
			store.cladefs.recordHeldBases("darwin-node", "arm64", [darwinLeak], [], 2_000, 0n);
			expect(store.cladefs.latestSharedBase("cladefs", "arm64", 0n, "vm")?.cell?.cell).toBe(2002n);

			store.cladefs.purgeNodeSharedBases("darwin-node");

			expect(store.cladefs.latestSharedBase("cladefs", "arm64", 0n, "vm")?.cell?.cell).toBe(1001n);
		});
	});
});
