import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { INTERNAL_AUTH_HEADER, INTERNAL_ORG_HEADER, type CallerIdentity } from "../src/auth.ts";
import type { ControllerDO, ControllerEnv } from "../src/controller-do.ts";
import type { TestOverrides } from "../src/test-harness.ts";

function ownerCaller(login = "ramon@echophase.com", orgId = "org_board_test"): CallerIdentity {
	return {
		role: "owner",
		nodeId: `workos:${login}`,
		hostname: login,
		login,
		userId: `user_${login}`,
		orgId,
		orgRole: "admin",
		authMethod: "workos-jwt",
	};
}

function memberCaller(login = "alice@echophase.com", orgId = "org_board_test"): CallerIdentity {
	return {
		role: "owner",
		nodeId: `workos:${login}`,
		hostname: login,
		login,
		userId: `user_${login}`,
		orgId,
		orgRole: "member",
		authMethod: "workos-jwt",
	};
}

async function rpc<T = Record<string, unknown>>(
	doName: string,
	proc: string,
	body: Record<string, unknown>,
	caller: CallerIdentity = ownerCaller(),
): Promise<{ status: number; data: T }> {
	const testEnv = env as unknown as ControllerEnv;
	const stub = testEnv.CONTROLLER.getByName(doName);
	const res = await stub.handleHttpRequest(
		new Request(`https://controller.test${proc}`, {
			method: "POST",
			headers: {
				"Content-Type": "application/json",
				[INTERNAL_AUTH_HEADER]: JSON.stringify(caller),
				[INTERNAL_ORG_HEADER]: caller.orgId,
			},
			body: JSON.stringify(body),
		}),
	);
	const data = (await res.json()) as T;
	return { status: res.status, data };
}

async function doHttp(
	doName: string,
	path: string,
	init: RequestInit = {},
	caller?: CallerIdentity,
): Promise<Response> {
	const testEnv = env as unknown as ControllerEnv;
	const stub = testEnv.CONTROLLER.getByName(doName);
	const headers = new Headers(init.headers);
	if (caller) {
		headers.set(INTERNAL_AUTH_HEADER, JSON.stringify(caller));
		headers.set(INTERNAL_ORG_HEADER, caller.orgId);
	}
	return stub.handleHttpRequest(
		new Request(`https://controller.test${path}`, { ...init, headers }),
	);
}

async function seedProject(doName: string, name: string): Promise<void> {
	const res = await rpc(doName, "/hotbed.v1.ClusterService/AddProject", {
		name,
		repos: [`https://github.com/org/${name}.git`],
	});
	expect(res.status).toBe(200);
}

async function advanceClock(doName: string, ms: number): Promise<void> {
	const testEnv = env as unknown as ControllerEnv;
	const stub = testEnv.CONTROLLER.getByName(doName);
	await runInDurableObject(stub, (instance: ControllerDO) => {
		(instance as unknown as { test: TestOverrides }).test.nowOffsetMs += ms;
	});
}

async function tickBoard(doName: string): Promise<void> {
	const testEnv = env as unknown as ControllerEnv;
	const stub = testEnv.CONTROLLER.getByName(doName);
	await runInDurableObject(stub, async (instance: ControllerDO) => {
		await instance.runMaintenance();
	});
}

describe("BoardService", () => {
	it("supports threads, replies, after precondition, edits, revisions, labels CAS, reactions, mentions, and schemas", async () => {
		const doName = "board-core-test";
		await seedProject(doName, "hotbed");
		const ramon = ownerCaller("ramon@echophase.com");
		const alice = memberCaller("alice@echophase.com");

		// Post a root message with a built-in git-commit@1 block and labels
		const sha = "0123456789abcdef0123456789abcdef01234567";
		const postText = [
			"Shipped native board!",
			"",
			"```json#git-commit@1",
			`{"sha":"${sha}","title":"feat: native board"}`,
			"```",
		].join("\n");
		const p1 = await rpc<{ message: { id: string; revision: string; blocks: unknown[] } }>(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{
				project: "hotbed",
				text: postText,
				labels: { status: "open", area: "controller" },
				idempotencyKey: "k-post-1",
			},
			ramon,
		);
		expect(p1.status).toBe(200);
		expect(p1.data.message.id).toBe("1");
		expect(p1.data.message.revision).toBe("1");
		expect(p1.data.message.blocks).toHaveLength(1);

		// Idempotent replay returns same message
		const p1Dup = await rpc<{ message: { id: string } }>(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{
				project: "hotbed",
				text: postText,
				idempotencyKey: "k-post-1",
			},
			ramon,
		);
		expect(p1Dup.status).toBe(200);
		expect(p1Dup.data.message.id).toBe("1");

		// Reply mentioning @ramon with afterId precondition
		const r1 = await rpc<{
			message: { id: string };
			mentions: Array<{ actor?: { handle: string } }>;
		}>(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{
				re: "1",
				text: "Looks great @ramon!",
				afterId: "1",
			},
			alice,
		);
		expect(r1.status).toBe(200);
		expect(r1.data.message.id).toBe("2");
		expect(r1.data.mentions[0]?.actor?.handle).toBe("ramon");

		// Stale afterId precondition fails with conflict
		const staleReply = await rpc(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{ re: "1", text: "stale", afterId: "1" },
			alice,
		);
		expect(staleReply.status).not.toBe(200);

		// Conditional label CAS
		const badCas = await rpc(
			doName,
			"/hotbed.v1.BoardService/LabelBoardMessage",
			{
				id: "1",
				set: { status: "shipped" },
				ifEquals: { status: "closed" },
			},
			ramon,
		);
		expect(badCas.status).not.toBe(200);

		const okCas = await rpc<{ labels: Record<string, string> }>(
			doName,
			"/hotbed.v1.BoardService/LabelBoardMessage",
			{
				id: "1",
				set: { status: "shipped" },
				ifEquals: { status: "open" },
			},
			ramon,
		);
		expect(okCas.status).toBe(200);
		expect(okCas.data.labels.status).toBe("shipped");

		// Emoji reaction normalization (thumbsup and 👍 both canonicalize to thumbs_up)
		const react1 = await rpc<{
			reactions: Array<{ emoji: string; actors: string[] }>;
		}>(doName, "/hotbed.v1.BoardService/ReactBoardMessage", { id: "1", emoji: "thumbsup" }, alice);
		expect(react1.status).toBe(200);
		expect(react1.data.reactions[0]?.emoji).toBe("thumbs_up");
		expect(react1.data.reactions[0]?.actors).toHaveLength(1);

		const react2 = await rpc<{
			reactions: Array<{ emoji: string; actors: string[] }>;
		}>(doName, "/hotbed.v1.BoardService/ReactBoardMessage", { id: "1", emoji: "👍" }, ramon);
		expect(react2.status).toBe(200);
		expect(react2.data.reactions[0]?.emoji).toBe("thumbs_up");
		expect(react2.data.reactions[0]?.actors).toHaveLength(2);

		// Edit with revision check
		const badEdit = await rpc(
			doName,
			"/hotbed.v1.BoardService/EditBoardMessage",
			{ id: "1", text: "updated", ifRevision: 99 },
			ramon,
		);
		expect(badEdit.status).not.toBe(200);

		const okEdit = await rpc<{ message: { revision: string } }>(
			doName,
			"/hotbed.v1.BoardService/EditBoardMessage",
			{ id: "1", text: "Shipped native board v2!", ifRevision: 1 },
			ramon,
		);
		expect(okEdit.status).toBe(200);
		expect(Number(okEdit.data.message.revision)).toBeGreaterThan(1);

		// Custom schema registration & validation
		const schemaRes = await rpc<{ schema: { id: string } }>(
			doName,
			"/hotbed.v1.BoardService/CreateBoardSchema",
			{
				name: "bench-result",
				version: 1,
				description: "Benchmark timing",
				schemaJson: JSON.stringify({
					type: "object",
					properties: { p95Ms: { type: "number", minimum: 0 } },
					required: ["p95Ms"],
				}),
			},
			ramon,
		);
		expect(schemaRes.status).toBe(200);
		expect(schemaRes.data.schema.id).toBe("bench-result@1");

		const badBlock = await rpc(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{
				project: "hotbed",
				text: '```json#bench-result@1\n{"p95Ms": -5}\n```',
			},
			ramon,
		);
		expect(badBlock.status).not.toBe(200);

		const goodBlock = await rpc<{
			message: { id: string; blocks: Array<{ schema: string; version: number }> };
		}>(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{
				project: "hotbed",
				text: '```json#bench-result@1\n{"p95Ms": 12.5}\n```',
			},
			ramon,
		);
		expect(goodBlock.status).toBe(200);
		expect(goodBlock.data.message.blocks[0]?.schema).toBe("bench-result");
		expect(goodBlock.data.message.blocks[0]?.version).toBe(1);

		// Delete message with reply leaves tombstone
		const delRes = await rpc<{ deleted: boolean }>(
			doName,
			"/hotbed.v1.BoardService/DeleteBoardMessage",
			{ id: "1" },
			ramon,
		);
		expect(delRes.status).toBe(200);
		expect(delRes.data.deleted).toBe(true);

		// List threads & check project counts in Snapshot
		const snap = await rpc<{
			projects: Array<{ name: string; boardThreads?: number }>;
		}>(doName, "/hotbed.v1.ClusterService/GetSnapshot", {}, ramon);
		expect(snap.status).toBe(200);
		const hp = snap.data.projects.find((p) => p.name === "hotbed");
		expect(hp?.boardThreads).toBe(1);
	});

	it("supports task DAG, claims held by identity, renew preserving original lease, expiry, auto-hold on non-owner edit, and cycle rejection", async () => {
		const doName = "board-tasks-test";
		await seedProject(doName, "cladefs");
		const ramon = ownerCaller("ramon@echophase.com");
		const alice = memberCaller("alice@echophase.com");

		// Batch create 2 tasks: b needs a
		const batch = await rpc<{
			tasks: Array<{ id: string; task: { state: string; needs: string[] } }>;
		}>(
			doName,
			"/hotbed.v1.BoardService/BatchCreateBoardTasks",
			{
				project: "cladefs",
				idempotencyKey: "plan-1",
				tasks: [
					{ key: "a", text: "Task A: build index" },
					{ key: "b", text: "Task B: run query", needs: [{ key: "a" }] },
				],
			},
			ramon,
		);
		expect(batch.status).toBe(200);
		expect(batch.data.tasks).toHaveLength(2);
		const taskAId = batch.data.tasks[0].id;
		const taskBId = batch.data.tasks[1].id;
		expect(batch.data.tasks[0].task.state).toBe("BOARD_TASK_STATE_READY");
		expect(batch.data.tasks[1].task.state).toBe("BOARD_TASK_STATE_WAITING");

		// Cycle rejection: A cannot need B
		const cycle = await rpc(
			doName,
			"/hotbed.v1.BoardService/SetBoardTaskNeeds",
			{ id: taskAId, needs: [taskBId] },
			ramon,
		);
		expect(cycle.status).not.toBe(200);

		// Claim Task A with custom 120s lease
		const claim = await rpc<{
			task: { task: { state: string } };
			claim: { epoch: number; until: string };
		}>(doName, "/hotbed.v1.BoardService/ClaimBoardTask", { id: taskAId, leaseSeconds: 120 }, alice);
		expect(claim.status).toBe(200);
		expect(claim.data.claim).toEqual({ epoch: 1, until: claim.data.claim.until });
		const until1 = Number(claim.data.claim.until);

		// Advance clock 60s and renew with leaseSeconds=0 -> preserves original 120s lease (+60s over until1)
		await advanceClock(doName, 60_000);
		const renewed = await rpc<{
			claim: { until: string };
		}>(doName, "/hotbed.v1.BoardService/RenewBoardTask", { id: taskAId, leaseSeconds: 0 }, alice);
		expect(renewed.status).toBe(200);
		const deltaMs = Number(renewed.data.claim.until) - until1;
		expect(deltaMs).toBeGreaterThanOrEqual(55_000);
		expect(deltaMs).toBeLessThanOrEqual(65_000);

		// Only the holder (principal + profile) settles a claim: not another principal,
		// nor the same principal under another profile.
		for (const [who, asProfile] of [
			[ramon, ""],
			[alice, "other-profile"],
		] as const) {
			const notHolder = await rpc<{ message: string }>(
				doName,
				"/hotbed.v1.BoardService/CompleteBoardTask",
				{ id: taskAId, asProfile },
				who,
			);
			expect(notHolder.status).toBe(409);
			expect(notHolder.data.message).toContain(
				`does not hold #${taskAId}: it is claimed by user:alice@echophase.com`,
			);
		}

		// Complete Task A -> Task B automatically becomes READY
		const done = await rpc<{ task: { state: string } }>(
			doName,
			"/hotbed.v1.BoardService/CompleteBoardTask",
			{ id: taskAId, result: "index built" },
			alice,
		);
		expect(done.status).toBe(200);
		expect(done.data.task.state).toBe("BOARD_TASK_STATE_COMPLETED");

		const bThread = await rpc<{ message: { task: { state: string } } }>(
			doName,
			"/hotbed.v1.BoardService/GetBoardThread",
			{ id: taskBId },
			ramon,
		);
		expect(bThread.status).toBe(200);
		expect(bThread.data.message.task.state).toBe("BOARD_TASK_STATE_READY");

		// Adopted thread: Alice posts a thread, Ramon adopts it as a task (Ramon = owner);
		// when Alice (non-owner author) edits the root text, the task is auto-held with kind=edited.
		const alicePost = await rpc<{ message: { id: string } }>(
			doName,
			"/hotbed.v1.BoardService/PostBoardMessage",
			{ project: "cladefs", text: "Investigate cache miss" },
			alice,
		);
		expect(alicePost.status).toBe(200);
		const adopted = await rpc<{ task: { id: string; task: { state: string } } }>(
			doName,
			"/hotbed.v1.BoardService/CreateBoardTask",
			{ re: alicePost.data.message.id },
			ramon,
		);
		expect(adopted.status).toBe(200);
		expect(adopted.data.task.task.state).toBe("BOARD_TASK_STATE_READY");

		const editHeld = await rpc<{
			message: { task: { state: string; blocked?: { kind: string } } };
		}>(
			doName,
			"/hotbed.v1.BoardService/EditBoardMessage",
			{ id: alicePost.data.message.id, text: "Investigate cache miss (edited spec)" },
			alice,
		);
		expect(editHeld.status).toBe(200);
		expect(editHeld.data.message.task.state).toBe("BOARD_TASK_STATE_BLOCKED");
		expect(editHeld.data.message.task.blocked?.kind).toBe("edited");

		// Owner unblocks it -> returns to READY
		const unblocked = await rpc<{ task: { state: string } }>(
			doName,
			"/hotbed.v1.BoardService/UnblockBoardTask",
			{ id: alicePost.data.message.id },
			ramon,
		);
		expect(unblocked.status).toBe(200);
		expect(unblocked.data.task.state).toBe("BOARD_TASK_STATE_READY");
	});

	it("handles chunked attachments, sandboxed HTTP download, schedules, and idempotent import", async () => {
		const doName = "board-attach-import-test";
		await seedProject(doName, "hotbed");
		const ramon = ownerCaller("ramon@echophase.com");

		// Upload a 600 KiB attachment (spans 2 chunks at 512 KiB)
		const payload = new Uint8Array(600 * 1024);
		for (let i = 0; i < payload.length; i++) payload[i] = i & 0xff;
		let bin = "";
		for (let i = 0; i < payload.length; i += 0x8000) {
			bin += String.fromCharCode(...payload.subarray(i, i + 0x8000));
		}
		const uploadRes = await rpc<{ id: string; size: string }>(
			doName,
			"/hotbed.v1.BoardService/UploadBoardAttachment",
			{
				name: "trace.bin",
				contentType: "application/octet-stream",
				data: btoa(bin),
			},
			ramon,
		);
		expect(uploadRes.status).toBe(200);
		const attId = uploadRes.data.id;
		expect(uploadRes.data.size).toBe(String(payload.length));

		// HTTP download returns exact bytes with sandbox CSP + nosniff
		const dlRes = await doHttp(doName, `/board/attachments/${attId}`, { method: "GET" }, ramon);
		expect(dlRes.status).toBe(200);
		expect(dlRes.headers.get("Content-Security-Policy")).toBe("sandbox");
		expect(dlRes.headers.get("X-Content-Type-Options")).toBe("nosniff");
		const dlBytes = new Uint8Array(await dlRes.arrayBuffer());
		expect(dlBytes.byteLength).toBe(payload.byteLength);
		expect(dlBytes[0]).toBe(0);
		expect(dlBytes[600 * 1024 - 1]).toBe((600 * 1024 - 1) & 0xff);

		// Scheduled board message fires when clock advances + tick-board runs
		const sched = await rpc<{ schedule: { id: string; state: string } }>(
			doName,
			"/hotbed.v1.BoardService/ScheduleBoardMessage",
			{
				project: "hotbed",
				text: "Reminder: check deploy",
				inSeconds: 60,
			},
			ramon,
		);
		expect(sched.status).toBe(200);
		expect(sched.data.schedule.state).toBe("BOARD_SCHEDULE_STATE_PENDING");

		await advanceClock(doName, 65_000);
		await tickBoard(doName);

		const listAfterTick = await rpc<{ threads: Array<{ message: { text: string } }> }>(
			doName,
			"/hotbed.v1.BoardService/ListBoardThreads",
			{ project: "hotbed" },
			ramon,
		);
		expect(
			listAfterTick.data.threads.some((t) => t.message.text === "Reminder: check deploy"),
		).toBe(true);

		// Import from external board preserving original IDs & resolving cross-message task needs
		const imp = await rpc<{
			messagesImported: number;
			tasksImported: number;
			idMap: Record<string, string>;
		}>(
			doName,
			"/hotbed.v1.BoardService/ImportBoard",
			{
				sourceUrl: "https://old-board.test/api/board",
				createProjects: true,
				finalizeNeeds: true,
				actors: [
					{
						id: "tailscale:user:ramon@echophase.com",
						kind: "BOARD_ACTOR_KIND_PERSON",
						handle: "ramon",
						name: "Ramon",
					},
				],
				messages: [
					{
						externalId: "14300",
						preferredId: "14300",
						project: "cladefs",
						text: "Imported task 1",
						by: {
							actor: "tailscale:user:ramon@echophase.com",
							principal: "tailscale:user:ramon@echophase.com",
						},
						createdAt: String(Date.now() - 10_000),
						task: { state: "BOARD_TASK_STATE_COMPLETED" },
					},
					{
						externalId: "14301",
						preferredId: "14301",
						project: "cladefs",
						text: "Imported task 2",
						by: {
							actor: "tailscale:user:ramon@echophase.com",
							principal: "tailscale:user:ramon@echophase.com",
						},
						createdAt: String(Date.now() - 5_000),
						task: {
							state: "BOARD_TASK_STATE_READY",
							needsExternalIds: ["14300"],
						},
					},
				],
			},
			ramon,
		);
		expect(imp.status).toBe(200);
		expect(imp.data.messagesImported).toBe(2);
		expect(imp.data.tasksImported).toBe(2);
		expect(imp.data.idMap["14300"]).toBe("14300");
		expect(imp.data.idMap["14301"]).toBe("14301");

		// Re-importing the same batch is idempotent
		const imp2 = await rpc<{ messagesImported: number; messagesSkipped: number }>(
			doName,
			"/hotbed.v1.BoardService/ImportBoard",
			{
				sourceUrl: "https://old-board.test/api/board",
				messages: [
					{
						externalId: "14300",
						preferredId: "14300",
						project: "cladefs",
						text: "Imported task 1",
						by: {
							actor: "tailscale:user:ramon@echophase.com",
							principal: "tailscale:user:ramon@echophase.com",
						},
					},
				],
			},
			ramon,
		);
		expect(imp2.status).toBe(200);
		expect(imp2.data.messagesImported ?? 0).toBe(0);
		expect(imp2.data.messagesSkipped).toBe(1);
	});

	it("lets a claim's holder re-claim, renew and settle it with no token, including an imported claim", async () => {
		const doName = "board-claim-identity-test";
		await seedProject(doName, "cladefs");
		const ramon = ownerCaller("ramon@echophase.com");
		const holder = "user:ramon@echophase.com+definite-bison";
		const until = Date.now() + 600_000;
		const imp = await rpc(
			doName,
			"/hotbed.v1.BoardService/ImportBoard",
			{
				messages: [
					{
						externalId: "14433",
						preferredId: "14433",
						project: "cladefs",
						text: "Imported claimed task",
						by: { actor: holder, principal: "user:ramon@echophase.com", profile: "definite-bison" },
						createdAt: String(Date.now() - 10_000),
						task: {
							state: "BOARD_TASK_STATE_CLAIMED",
							epoch: 1,
							claimActor: holder,
							claimUntil: String(until),
						},
					},
				],
			},
			ramon,
		);
		expect(imp.status).toBe(200);

		// Another profile of the same principal neither re-claims nor completes it.
		const otherClaim = await rpc<{ message: string }>(
			doName,
			"/hotbed.v1.BoardService/ClaimBoardTask",
			{ id: "14433", asProfile: "shining-shad" },
			ramon,
		);
		expect(otherClaim.status).toBe(409);
		expect(otherClaim.data.message).toBe(`task_claimed: #14433 is claimed by ${holder}`);

		// The holder re-claims it: same epoch, a fresh lease of the length asked for.
		await advanceClock(doName, 60_000);
		const reclaim = await rpc<{
			claim: { epoch: number; until: string };
			task: { task: { claim: { actor: string; leaseSeconds: number } } };
		}>(
			doName,
			"/hotbed.v1.BoardService/ClaimBoardTask",
			{ id: "14433", asProfile: "definite-bison", leaseSeconds: 1800 },
			ramon,
		);
		expect(reclaim.status).toBe(200);
		expect(reclaim.data.claim.epoch).toBe(1);
		expect(reclaim.data.task.task.claim).toEqual({
			actor: holder,
			until: reclaim.data.claim.until,
			leaseSeconds: 1800,
		});
		expect(Number(reclaim.data.claim.until)).toBeGreaterThan(until + 1_200_000);

		// Renew with no lease keeps the claim's own length.
		const renewed = await rpc<{ claim: { epoch: number; until: string } }>(
			doName,
			"/hotbed.v1.BoardService/RenewBoardTask",
			{ id: "14433", asProfile: "definite-bison" },
			ramon,
		);
		expect(renewed.status).toBe(200);
		expect(renewed.data.claim.epoch).toBe(1);

		// A worker naming a claim other than the current one (a stale run under the same
		// identity) is refused.
		const stale = await rpc<{ message: string }>(
			doName,
			"/hotbed.v1.BoardService/CompleteBoardTask",
			{ id: "14433", asProfile: "definite-bison", epoch: 2 },
			ramon,
		);
		expect(stale.status).toBe(409);
		expect(stale.data.message).toBe(
			"claim_lost: claim 2 on #14433 is over: #14433 was claimed again (claim 1)",
		);

		const done = await rpc<{ task: { state: string; result: string } }>(
			doName,
			"/hotbed.v1.BoardService/CompleteBoardTask",
			{ id: "14433", asProfile: "definite-bison", epoch: 1, result: "simulated" },
			ramon,
		);
		expect(done.status).toBe(200);
		expect(done.data.task).toMatchObject({
			state: "BOARD_TASK_STATE_COMPLETED",
			result: "simulated",
		});

		// Once settled, it is no longer held.
		const again = await rpc<{ message: string }>(
			doName,
			"/hotbed.v1.BoardService/ReleaseBoardTask",
			{ id: "14433", asProfile: "definite-bison" },
			ramon,
		);
		expect(again.status).toBe(409);
		expect(again.data.message).toBe(`claim_lost: ${holder} does not hold #14433: it is completed`);
	});
});
