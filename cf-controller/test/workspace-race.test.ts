import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vite-plus/test";
import type { ControllerContext } from "../src/context.ts";
import { ErrorCode, hotbedError } from "../src/errors.ts";
import {
	AdmissionSchema,
	AdmissionSignalSchema,
	CreateWorkspaceRequestSchema,
	HeartbeatRequestSchema,
	NodeSchema,
	ProjectSchema,
	RepoSchema,
	ResourcesSchema,
	SessionSchema,
	WorkspaceSchema,
	type Workspace,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { createWorkspaceService, discardRacedWorkspace } from "../src/services/workspaces.ts";
import { Wakeups } from "../src/wakeups.ts";

/** The parts of ControllerContext discardRacedWorkspace uses, recording what it did. */
function fakeController(opts: { releaseFails?: unknown } = {}) {
	const background: Promise<unknown>[] = [];
	const released: string[] = [];
	const rowsReleased: string[] = [];
	const tunnelCalls: Array<{ node: string; method: string }> = [];
	const ctrl = {
		waitUntil: (p: Promise<unknown>) => background.push(p),
		now: () => 1_000,
		releaseWorkspace: async (id: string) => {
			if (opts.releaseFails) throw opts.releaseFails;
			released.push(id);
		},
		tunnel: {
			callUnary: async (node: string, _host: string, method: string) => {
				tunnelCalls.push({ node, method });
				throw hotbedError(ErrorCode.NotFound, "no workspace");
			},
		},
		store: {
			workspaces: { release: (id: string) => rowsReleased.push(id) },
			daemonLogs: { log: () => {} },
		},
	} as unknown as ControllerContext;
	return { ctrl, background, released, rowsReleased, tunnelCalls };
}

const loser = { host: "slow-host", view: { node: { id: "node-slow" } } };

function deferred() {
	let resolve!: (v: { ws?: Workspace; err?: unknown }) => void;
	const promise = new Promise<{ ws?: Workspace; err?: unknown }>((r) => (resolve = r));
	return { promise, resolve };
}

describe("discardRacedWorkspace", () => {
	it("aborts the loser and releases what it made as background work the response does not wait for", async () => {
		const f = fakeController();
		const ac = new AbortController();
		const creating = deferred();

		discardRacedWorkspace(f.ctrl, loser, "ws-lost", ac, creating.promise);

		expect(ac.signal.aborted).toBe(true);
		expect(f.background).toHaveLength(1);
		// The winner's response goes out now; the loser has not settled yet.
		expect(f.released).toEqual([]);

		creating.resolve({ ws: create(WorkspaceSchema, { id: "ws-lost" }) });
		await Promise.all(f.background);
		expect(f.released).toEqual(["ws-lost"]);
		expect(f.tunnelCalls).toEqual([]);
	});

	it("releases by ID a workspace the worker may have made as its creation was aborted", async () => {
		const f = fakeController();
		const creating = deferred();
		discardRacedWorkspace(f.ctrl, loser, "ws-aborted", new AbortController(), creating.promise);
		creating.resolve({ err: new Error("aborted") });
		await Promise.all(f.background);
		expect(f.tunnelCalls).toEqual([
			{ node: "node-slow", method: "/hotbed.v1.WorkspaceService/ReleaseWorkspace" },
		]);
	});

	it("releases the row of a loser whose worker went away: its heartbeat releases the rest", async () => {
		const f = fakeController({
			releaseFails: hotbedError(ErrorCode.WorkerUnreachable, "slow-host is offline"),
		});
		const creating = deferred();
		discardRacedWorkspace(f.ctrl, loser, "ws-gone", new AbortController(), creating.promise);
		creating.resolve({ ws: create(WorkspaceSchema, { id: "ws-gone" }) });
		await Promise.all(f.background);
		expect(f.rowsReleased).toEqual(["ws-gone"]);
	});
});

describe("createWhenFree max queue wait", () => {
	it("cancels with an error once maxQueueWaitMs elapses while all nodes stay busy", async () => {
		const busyNode = (id: string, hostname: string) => ({
			node: create(NodeSchema, { id, hostname, online: true }),
			online: true,
			tunnelConnected: true,
			hb: create(HeartbeatRequestSchema, {
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: 32n * 1024n ** 3n,
					runtimes: ["vm"],
				}),
				admission: create(AdmissionSchema, {
					holding: true,
					reason: "CPU pressure 90%",
					signals: [create(AdmissionSignalSchema, { name: "cpu-pressure", holding: true })],
				}),
			}),
			commands: [],
			bases: [],
			workspaces: [],
		});
		const ctrl = {
			now: () => 10_000,
			placementWaitCapMs: 20,
			maxQueueWaitMs: 60,
			nodeChanges: new Wakeups(),
			capacityChanges: new Wakeups(),
			sessionRaces: new Map(),
			creatingWS: new Map(),
			cladefsTenantId: null,
			follows: () => 0,
			views: () => [busyNode("n1", "ubu1"), busyNode("n2", "vm1")],
			withPlacementLock: async <T>(fn: () => T) => fn(),
			store: {
				fleet: {
					addPlacement: () => {},
				},
				sessions: {
					live: (id: string) => create(SessionSchema, { id, project: "hotbed", agent: "agent" }),
				},
				projects: {
					get: (name: string) =>
						create(ProjectSchema, { name, repos: ["github.com/semistrict/hotbed"] }),
					listRepos: () => [
						create(RepoSchema, { id: "hotbed", url: "https://github.com/semistrict/hotbed.git" }),
					],
				},
				workspaces: {
					ofSession: () => [],
				},
				cladefs: {
					hasSharedBase: () => false,
				},
			},
		} as unknown as ControllerContext;

		const { helpers } = createWorkspaceService(ctrl);
		await expect(
			helpers.createWhenFree(
				create(CreateWorkspaceRequestSchema, {
					sessionId: "busy-owl",
					waitForCapacity: true,
				}),
			),
		).rejects.toThrow(/nothing can queue for more than 15m/);
	});
});
