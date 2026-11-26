import { env, SELF } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { INTERNAL_AUTH_HEADER, INTERNAL_ORG_HEADER, type CallerIdentity } from "../src/auth.ts";
import { VIEWER_ALLOWED_PROCEDURES } from "../src/connect-adapter.ts";

function viewerCaller(orgId = "org_viewer_test"): CallerIdentity {
	return {
		role: "owner",
		nodeId: "workos:user_viewer_1",
		hostname: "viewer@acme.test",
		login: "viewer@acme.test",
		userId: "user_viewer_1",
		orgId,
		orgRole: "viewer",
		addr: "",
		authMethod: "workos-jwt",
	};
}

function memberCaller(orgId = "org_viewer_test"): CallerIdentity {
	return {
		role: "owner",
		nodeId: "workos:user_member_1",
		hostname: "member@acme.test",
		login: "member@acme.test",
		userId: "user_member_1",
		orgId,
		orgRole: "member",
		addr: "",
		authMethod: "workos-jwt",
	};
}

async function doFetch(doName: string, path: string, init: RequestInit = {}): Promise<Response> {
	const stub = env.CONTROLLER.getByName(doName);
	return stub.fetch(new Request(`https://controller.test${path}`, init));
}

function connectHeaders(caller: CallerIdentity): HeadersInit {
	return {
		"Content-Type": "application/json",
		[INTERNAL_AUTH_HEADER]: JSON.stringify(caller),
		[INTERNAL_ORG_HEADER]: caller.orgId,
	};
}

describe("viewer role enforcement", () => {
	const doName = "viewer-role-test";

	it("allows read-only ConnectRPC calls for viewer", async () => {
		const viewer = viewerCaller();
		for (const proc of [
			"/hotbed.v1.ClusterService/ListNodes",
			"/hotbed.v1.ClusterService/ListProjects",
			"/hotbed.v1.ClusterService/ListRepos",
			"/hotbed.v1.ClusterService/GetSnapshot",
			"/hotbed.v1.WorkspaceService/ListWorkspaces",
			"/hotbed.v1.QuestionService/ListQuestionProjects",
		]) {
			expect(VIEWER_ALLOWED_PROCEDURES.has(proc)).toBe(true);
			const res = await doFetch(doName, proc, {
				method: "POST",
				headers: connectHeaders(viewer),
				body: "{}",
			});
			expect(res.status, `${proc} should succeed for viewer`).toBe(200);
		}
		for (const proc of [
			"/hotbed.v1.SessionService/ListSessions",
			"/hotbed.v1.QuestionService/ListQuestions",
			"/hotbed.v1.MemoryService/ListMemories",
			"/hotbed.v1.ScheduleService/ListSchedules",
			"/hotbed.v1.ReviewService/ListReviews",
		]) {
			expect(VIEWER_ALLOWED_PROCEDURES.has(proc)).toBe(true);
			const res = await doFetch(doName, proc, {
				method: "POST",
				headers: connectHeaders(viewer),
				body: "{}",
			});
			expect(res.status, `${proc} should not be forbidden for viewer`).not.toBe(403);
		}
	});

	it("denies mutating ConnectRPC calls for viewer", async () => {
		const viewer = viewerCaller();
		for (const proc of [
			"/hotbed.v1.ClusterService/AddProject",
			"/hotbed.v1.ClusterService/PutRepo",
			"/hotbed.v1.ClusterService/RetireNode",
			"/hotbed.v1.SessionService/StartSession",
			"/hotbed.v1.WorkspaceService/RunCommand",
			"/hotbed.v1.QuestionService/AddQuestions",
			"/hotbed.v1.MemoryService/ProposeMemory",
			"/hotbed.v1.ScheduleService/PutSchedule",
			"/hotbed.v1.ReviewService/RequestReview",
			"/hotbed.v1.GitAuthService/MintGitToken",
		]) {
			expect(VIEWER_ALLOWED_PROCEDURES.has(proc)).toBe(false);
			const res = await doFetch(doName, proc, {
				method: "POST",
				headers: connectHeaders(viewer),
				body: "{}",
			});
			expect(res.status, `${proc} should be forbidden for viewer`).toBe(403);
			const body = (await res.json()) as { message?: string };
			expect(body.message).toContain("viewer role is read-only");
		}
	});

	it("denies worker token minting and join token mutations for viewer, while allowing GET join-tokens", async () => {
		const viewer = viewerCaller();
		const member = memberCaller();

		// Viewer can list join tokens
		const listRes = await doFetch(doName, "/admin/join-tokens", {
			method: "GET",
			headers: connectHeaders(viewer),
		});
		expect(listRes.status).toBe(200);

		// Viewer cannot mint a join token
		const mintJoinRes = await doFetch(doName, "/admin/join-tokens", {
			method: "POST",
			headers: connectHeaders(viewer),
			body: JSON.stringify({ label: "blocked" }),
		});
		expect(mintJoinRes.status).toBe(403);

		// Member can mint a join token, viewer cannot revoke it
		const memberMint = await doFetch(doName, "/admin/join-tokens", {
			method: "POST",
			headers: connectHeaders(member),
			body: JSON.stringify({ label: "allowed" }),
		});
		expect(memberMint.status).toBe(201);
		const minted = (await memberMint.json()) as { id: string };

		const revokeRes = await doFetch(doName, `/admin/join-tokens/${minted.id}`, {
			method: "DELETE",
			headers: connectHeaders(viewer),
		});
		expect(revokeRes.status).toBe(403);

		// Viewer cannot mint a worker token
		const workerTokRes = await doFetch(doName, "/admin/worker-token", {
			method: "POST",
			headers: connectHeaders(viewer),
			body: JSON.stringify({ nodeId: "n1", hostname: "h1" }),
		});
		expect(workerTokRes.status).toBe(403);
	});

	it("preserves viewer role on personal API tokens minted by a viewer", async () => {
		const viewer = viewerCaller("org_viewer_api");
		const apiDo = "viewer-role-api-test";

		// Viewer mints a personal API token
		const createRes = await doFetch(apiDo, "/admin/api-tokens", {
			method: "POST",
			headers: connectHeaders(viewer),
			body: JSON.stringify({ name: "ci-readonly" }),
		});
		expect(createRes.status).toBe(201);
		const created = (await createRes.json()) as { token: string; orgRole: string };
		expect(created.orgRole).toBe("viewer");

		// Using that API token: read-only RPC succeeds, mutating RPC is denied
		const readRes = await doFetch(apiDo, "/hotbed.v1.ClusterService/ListNodes", {
			method: "POST",
			headers: {
				"Content-Type": "application/json",
				Authorization: `Bearer ${created.token}`,
			},
			body: "{}",
		});
		expect(readRes.status).toBe(200);

		const writeRes = await doFetch(apiDo, "/hotbed.v1.ClusterService/AddProject", {
			method: "POST",
			headers: {
				"Content-Type": "application/json",
				Authorization: `Bearer ${created.token}`,
			},
			body: "{}",
		});
		expect(writeRes.status).toBe(403);
	});

	it("SELF smoke check", async () => {
		const res = await SELF.fetch("https://controller.test/healthz");
		expect(res.status).toBe(200);
	});
});
