import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import {
	CreateWorkspaceRequestSchema,
	HostedProjectSchema,
	PlacementSchema,
	SharedBaseSchema,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { shouldCaptureProjectBase } from "./workspaces.ts";

describe("shouldCaptureProjectBase", () => {
	const nowMs = 1_800_000_000_000;
	const intervalMs = 30 * 60_000;
	const hosted = create(HostedProjectSchema, { name: "hotbed", scratchEpoch: 1n });
	const freshBase = {
		seenAt: nowMs - 5 * 60_000,
		base: create(SharedBaseSchema, {
			scratchEpoch: 1n,
			capturedAt: BigInt(nowMs - 5 * 60_000),
			baseCommitTime: 1_700_000_000n,
			repos: { hotbed: "aaaa" },
		}),
	};
	const staleBase = {
		seenAt: nowMs - 45 * 60_000,
		base: create(SharedBaseSchema, {
			scratchEpoch: 1n,
			capturedAt: BigInt(nowMs - 45 * 60_000),
			baseCommitTime: 1_700_000_000n,
			repos: { hotbed: "aaaa" },
		}),
	};

	it("captures when no shared base exists yet", () => {
		const req = create(CreateWorkspaceRequestSchema, { repo: "hotbed", syncDir: "/repo" });
		expect(
			shouldCaptureProjectBase({
				req,
				hosted,
				latest: undefined,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(true);
	});

	it("skips capture when another session is already capturing in flight", () => {
		const req = create(CreateWorkspaceRequestSchema, { repo: "hotbed", syncDir: "/repo" });
		expect(
			shouldCaptureProjectBase({
				req,
				hosted,
				latest: undefined,
				inFlight: true,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);
	});

	it("keeps routine sessions ephemeral while the shared base is fresh", () => {
		const req = create(CreateWorkspaceRequestSchema, {
			repo: "hotbed",
			syncDir: "/repo",
			baseCommit: "bbbb",
			baseCommitTime: 1_700_001_000n,
		});
		expect(
			shouldCaptureProjectBase({
				req,
				hosted,
				latest: freshBase,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);
	});

	it("captures when the shared base is stale and code moved forward", () => {
		const req = create(CreateWorkspaceRequestSchema, {
			repo: "hotbed",
			syncDir: "/repo",
			baseCommit: "bbbb",
			baseCommitTime: 1_700_001_000n,
		});
		expect(
			shouldCaptureProjectBase({
				req,
				hosted,
				latest: staleBase,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(true);
	});

	it("never regresses the shared base from an older branch", () => {
		const req = create(CreateWorkspaceRequestSchema, {
			repo: "hotbed",
			syncDir: "/repo",
			baseCommit: "0000",
			baseCommitTime: 1_699_000_000n,
		});
		expect(
			shouldCaptureProjectBase({
				req,
				hosted,
				latest: staleBase,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);
	});

	it("respects --no-capture, --bench, --fork, and explicit --capture-base", () => {
		expect(
			shouldCaptureProjectBase({
				req: create(CreateWorkspaceRequestSchema, { repo: "hotbed", noCapture: true }),
				hosted,
				latest: undefined,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);

		expect(
			shouldCaptureProjectBase({
				req: create(CreateWorkspaceRequestSchema, {
					repo: "hotbed",
					placement: create(PlacementSchema, { bench: true }),
				}),
				hosted,
				latest: undefined,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);

		expect(
			shouldCaptureProjectBase({
				req: create(CreateWorkspaceRequestSchema, { repo: "hotbed", throwaway: true }),
				hosted,
				latest: undefined,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(false);

		expect(
			shouldCaptureProjectBase({
				req: create(CreateWorkspaceRequestSchema, { repo: "hotbed", captureBase: true }),
				hosted,
				latest: freshBase,
				inFlight: true,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(true);
	});

	it("captures after project rebuild bumps scratch_epoch", () => {
		const rebuilt = create(HostedProjectSchema, { name: "hotbed", scratchEpoch: 2n });
		expect(
			shouldCaptureProjectBase({
				req: create(CreateWorkspaceRequestSchema, { repo: "hotbed", syncDir: "/repo" }),
				hosted: rebuilt,
				latest: freshBase,
				inFlight: false,
				captureIntervalMs: intervalMs,
				nowMs,
			}),
		).toBe(true);
	});
});
