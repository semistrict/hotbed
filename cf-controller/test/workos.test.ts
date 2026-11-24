import { env } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
	ClusterService,
	HeartbeatRequestSchema,
	NodeUsageSchema,
	RepoSchema,
	ResourcesSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import worker from "../src/index.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import { base64UrlEncode, pkceS256, resetWorkOSJwksCache, type WorkOSEnv } from "../src/workos.ts";

const WORKOS_ORIGIN = "https://mock-workos.test";
const WORKOS_CLIENT_ID = "client_test_01";
const WORKOS_API_KEY = "sk_test_secret_key_1234567890";
const WORKOS_COOKIE_PASSWORD = "super-secret-cookie-password-at-least-32-chars";

interface TestKeyPair {
	kid: string;
	privateKey: CryptoKey;
	publicJwk: JsonWebKey & { kid: string; alg: string; use: string };
}

let testKey: TestKeyPair;

async function generateTestKey(): Promise<TestKeyPair> {
	const pair = (await crypto.subtle.generateKey(
		{
			name: "RSASSA-PKCS1-v1_5",
			modulusLength: 2048,
			publicExponent: new Uint8Array([1, 0, 1]),
			hash: "SHA-256",
		},
		true,
		["sign", "verify"],
	)) as CryptoKeyPair;
	const pub = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
	const kid = "kid_test_1";
	return {
		kid,
		privateKey: pair.privateKey,
		publicJwk: { ...pub, kid, alg: "RS256", use: "sig" },
	};
}

async function signJwt(claims: Record<string, unknown>): Promise<string> {
	const enc = new TextEncoder();
	const header = base64UrlEncode(
		enc.encode(JSON.stringify({ alg: "RS256", typ: "JWT", kid: testKey.kid })),
	);
	const payload = base64UrlEncode(enc.encode(JSON.stringify(claims)));
	const sig = new Uint8Array(
		await crypto.subtle.sign(
			"RSASSA-PKCS1-v1_5",
			testKey.privateKey,
			enc.encode(`${header}.${payload}`),
		),
	);
	return `${header}.${payload}.${base64UrlEncode(sig)}`;
}

function makeWorkOSEnv(overrides?: Partial<ControllerEnv & WorkOSEnv>): ControllerEnv {
	return {
		...(env as unknown as ControllerEnv),
		OWNER_TOKEN: "",
		JOIN_TOKEN: "",
		WORKOS_CLIENT_ID,
		WORKOS_API_KEY,
		WORKOS_COOKIE_PASSWORD,
		WORKOS_AUTH_PROVIDER: "authkit",
		WORKOS_API_BASE: WORKOS_ORIGIN,
		ALLOW_LOCAL_REPOS: "1",
		...overrides,
	};
}

function callWorker(testEnv: ControllerEnv, input: string, init?: RequestInit): Promise<Response> {
	return worker.fetch(new Request(input, init), testEnv);
}

function makeRpcClient(
	testEnv: ControllerEnv,
	token?: string,
	extraHeaders?: Record<string, string>,
) {
	const authInterceptor: Interceptor = (next) => async (req) => {
		if (token) req.header.set("Authorization", `Bearer ${token}`);
		for (const [k, v] of Object.entries(extraHeaders ?? {})) {
			req.header.set(k, v);
		}
		return next(req);
	};
	const transport = createTransport({
		httpClient: createFetchClient((input, init) => worker.fetch(new Request(input, init), testEnv)),
		baseUrl: "https://controller.test",
		useBinaryFormat: true,
		interceptors: [authInterceptor],
		sendCompression: null,
		acceptCompression: [],
		compressMinBytes: 0,
		readMaxBytes: 0xffffffff,
		writeMaxBytes: 0xffffffff,
	});
	return createClient(ClusterService, transport);
}

function extractCookies(res: Response): string[] {
	const headers = res.headers as Headers & { getSetCookie?: () => string[] };
	if (typeof headers.getSetCookie === "function") {
		return headers.getSetCookie();
	}
	const single = res.headers.get("Set-Cookie");
	return single ? [single] : [];
}

function cookiePair(setCookie: string): string {
	return setCookie.split(";")[0];
}

type MockFetchHandler = (url: URL, init?: RequestInit) => Response | Promise<Response> | null;

describe("WorkOS multi-organization, multi-user, per-org ControllerDO integration", () => {
	let mockHandlers: MockFetchHandler[] = [];

	beforeAll(async () => {
		testKey = await generateTestKey();
	});

	beforeEach(() => {
		resetWorkOSJwksCache();
		mockHandlers = [
			(url) => {
				if (url.origin === WORKOS_ORIGIN && url.pathname === `/sso/jwks/${WORKOS_CLIENT_ID}`) {
					return Response.json({ keys: [testKey.publicJwk] });
				}
				return null;
			},
		];
		const origFetch = globalThis.fetch;
		vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
			const reqUrl =
				typeof input === "string"
					? new URL(input)
					: input instanceof URL
						? input
						: new URL(input.url);
			if (reqUrl.origin === WORKOS_ORIGIN) {
				for (const h of mockHandlers) {
					const res = await h(reqUrl, init);
					if (res) return res;
				}
				return Response.json(
					{
						message: `unmocked WorkOS request ${init?.method ?? "GET"} ${reqUrl.pathname}${reqUrl.search}`,
					},
					{ status: 500 },
				);
			}
			return origFetch(input, init);
		});
	});

	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("isolates each WorkOS organization into its own ControllerDO and supports multiple users per org", async () => {
		const testEnv = makeWorkOSEnv();
		const nowSec = Math.floor(Date.now() / 1000);

		const aliceOrgAToken = await signJwt({
			sub: "user_alice",
			email: "alice@acme.test",
			org_id: "org_acme",
			role: "admin",
			sid: "sess_alice_1",
			exp: nowSec + 3600,
		});
		const bobOrgAToken = await signJwt({
			sub: "user_bob",
			email: "bob@acme.test",
			org_id: "org_acme",
			role: "member",
			sid: "sess_bob_1",
			exp: nowSec + 3600,
		});
		const carolOrgBToken = await signJwt({
			sub: "user_carol",
			email: "carol@globex.test",
			org_id: "org_globex",
			role: "admin",
			sid: "sess_carol_1",
			exp: nowSec + 3600,
		});

		const alice = makeRpcClient(testEnv, aliceOrgAToken, { "X-Hotbed-Host": "alice-laptop" });
		const bob = makeRpcClient(testEnv, bobOrgAToken, { "X-Hotbed-Host": "bob-laptop" });
		const carol = makeRpcClient(testEnv, carolOrgBToken, { "X-Hotbed-Host": "carol-laptop" });

		// Alice creates a repo in org_acme
		await alice.putRepo({
			repo: create(RepoSchema, {
				id: "acme-app",
				url: "https://github.com/acme/app.git",
				branch: "main",
			}),
		});

		// Bob (different user in the SAME org_acme) sees the exact same cluster & repo
		const aliceNodes = await alice.listNodes({});
		const bobNodes = await bob.listNodes({});
		const bobRepos = await bob.listRepos({});
		expect(bobNodes.clusterId).toBe(aliceNodes.clusterId);
		expect(bobRepos.repos.map((r) => r.id)).toEqual(["acme-app"]);

		// Carol (different org_globex) hits a completely separate ControllerDO with its own clusterId and empty repos
		const carolNodes = await carol.listNodes({});
		const carolRepos = await carol.listRepos({});
		expect(carolNodes.clusterId).not.toBe(aliceNodes.clusterId);
		expect(carolRepos.repos).toEqual([]);

		// Carol registers her own repo in org_globex; Alice & Bob do not see it
		await carol.putRepo({
			repo: create(RepoSchema, {
				id: "globex-core",
				url: "https://github.com/globex/core.git",
				branch: "main",
			}),
		});
		expect((await bob.listRepos({})).repos.map((r) => r.id)).toEqual(["acme-app"]);
		expect((await carol.listRepos({})).repos.map((r) => r.id)).toEqual(["globex-core"]);

		// Spoofing X-Hotbed-Internal-Auth / X-Hotbed-Internal-Org headers from untrusted clients fails closed
		const spoofedRes = await callWorker(
			testEnv,
			"https://controller.test/hotbed.v1.ClusterService/ListNodes",
			{
				method: "POST",
				headers: {
					"Content-Type": "application/json",
					"X-Hotbed-Internal-Org": "org_acme",
					"X-Hotbed-Internal-Auth": JSON.stringify({
						role: "owner",
						login: "attacker@evil.test",
						orgId: "org_acme",
					}),
				},
				body: "{}",
			},
		);
		expect(spoofedRes.status).toBe(403);

		// A WorkOS JWT with no org_id is rejected fail-closed
		const noOrgToken = await signJwt({
			sub: "user_no_org",
			email: "lonely@example.test",
			exp: nowSec + 3600,
		});
		const noOrgClient = makeRpcClient(testEnv, noOrgToken);
		await expect(noOrgClient.listNodes({})).rejects.toThrow(/no active organization selected/);

		// DEFAULT_ORG_ID maps an organization to the "default" DO cleanly
		const defaultEnv = makeWorkOSEnv({ DEFAULT_ORG_ID: "org_default_mapped" });
		const defaultOrgToken = await signJwt({
			sub: "user_default",
			email: "owner@default.test",
			org_id: "org_default_mapped",
			role: "admin",
			exp: nowSec + 3600,
		});
		const defClient = makeRpcClient(defaultEnv, defaultOrgToken);
		const defNodes = await defClient.listNodes({});
		const healthRes = await callWorker(defaultEnv, "https://controller.test/healthz");
		const health = (await healthRes.json()) as { clusterId: string; orgId?: string };
		expect(defNodes.clusterId).toBe(health.clusterId);
		expect(health.orgId).toBe("org_default_mapped");
	});

	it("mints org-scoped join tokens, exchanges them for node-bound worker tokens on Join, and isolates API tokens per org", async () => {
		const testEnv = makeWorkOSEnv();
		const nowSec = Math.floor(Date.now() / 1000);

		const aliceToken = await signJwt({
			sub: "user_alice",
			email: "alice@acme.test",
			org_id: "org_acme_tok",
			role: "admin",
			exp: nowSec + 3600,
		});
		const bobToken = await signJwt({
			sub: "user_bob",
			email: "bob@acme.test",
			org_id: "org_acme_tok",
			role: "member",
			exp: nowSec + 3600,
		});

		// 1. Mint an org-scoped worker join token
		const jtRes = await callWorker(testEnv, "https://controller.test/admin/join-tokens", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${aliceToken}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ label: "linux-fleet", expiresInHours: 24 }),
		});
		expect(jtRes.status).toBe(201);
		const jt = (await jtRes.json()) as { id: string; token: string; orgId: string };
		expect(jt.token.startsWith("hbj_org_acme_tok.")).toBe(true);
		expect(jt.orgId).toBe("org_acme_tok");

		// 2. Worker joins using the org-scoped join token and receives a node-bound org-scoped worker token
		const joiner = makeRpcClient(testEnv, jt.token, {
			"X-Hotbed-Node-Id": "node-acme-1",
			"X-Hotbed-Host": "acme-ubu1",
			"X-Hotbed-Addr": "100.64.1.10:7121",
		});
		const joined = await joiner.join({});
		expect(joined.workerToken.startsWith("hbw_org_acme_tok.")).toBe(true);
		expect(joined.node?.hostname).toBe("acme-ubu1");

		// 3. Worker heartbeats using the minted per-worker token
		const workerClient = makeRpcClient(testEnv, joined.workerToken, {
			"X-Hotbed-Node-Id": "node-acme-1",
			"X-Hotbed-Host": "acme-ubu1",
			"X-Hotbed-Addr": "100.64.1.10:7121",
		});
		const hb = await workerClient.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, { arch: "amd64", cpus: 8, memoryBytes: BigInt(16e9) }),
				usage: create(NodeUsageSchema, { cpuBusy: 0.1 }),
			}),
		);
		expect(hb).toBeTruthy();

		// Worker token cannot invoke owner RPCs
		await expect(
			workerClient.putRepo({
				repo: create(RepoSchema, { id: "bad", url: "https://github.com/x/y.git" }),
			}),
		).rejects.toThrow(/worker token is not authorized/);

		// Join token cannot open /ws/worker directly
		const wsBad = await callWorker(testEnv, "https://controller.test/ws/worker", {
			headers: { Upgrade: "websocket", Authorization: `Bearer ${jt.token}` },
		});
		expect(wsBad.status).toBe(403);

		// Revoking the join token blocks future joins while the already-joined worker keeps heartbeating
		const revRes = await callWorker(testEnv, `https://controller.test/admin/join-tokens/${jt.id}`, {
			method: "DELETE",
			headers: { Authorization: `Bearer ${aliceToken}` },
		});
		expect(revRes.status).toBe(200);
		await expect(joiner.join({})).rejects.toThrow(/invalid bearer token/);
		await expect(
			workerClient.heartbeat(create(HeartbeatRequestSchema, { clusterId: joined.clusterId })),
		).resolves.toBeTruthy();

		// 4. Personal org-scoped API tokens (`hbu_<org>.<secret>`)
		const apiRes = await callWorker(testEnv, "https://controller.test/admin/api-tokens", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${aliceToken}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ name: "ci-runner", expiresInDays: 30 }),
		});
		expect(apiRes.status).toBe(201);
		const apiTok = (await apiRes.json()) as { id: string; token: string };
		expect(apiTok.token.startsWith("hbu_org_acme_tok.")).toBe(true);

		// API token calls ConnectRPC into org_acme_tok's DO
		const apiClient = makeRpcClient(testEnv, apiTok.token);
		const nodesRes = await apiClient.listNodes({});
		expect(nodesRes.clusterId).toBe(joined.clusterId);
		expect(nodesRes.nodes.map((n) => n.hostname)).toContain("acme-ubu1");

		// Bob (non-admin member) cannot revoke Alice's API token
		const bobRevoke = await callWorker(
			testEnv,
			`https://controller.test/admin/api-tokens/${apiTok.id}`,
			{ method: "DELETE", headers: { Authorization: `Bearer ${bobToken}` } },
		);
		expect(bobRevoke.status).toBe(403);

		// Alice revokes her API token -> immediately refused
		const aliceRevoke = await callWorker(
			testEnv,
			`https://controller.test/admin/api-tokens/${apiTok.id}`,
			{ method: "DELETE", headers: { Authorization: `Bearer ${aliceToken}` } },
		);
		expect(aliceRevoke.status).toBe(200);
		await expect(apiClient.listNodes({})).rejects.toThrow(/invalid bearer token/);
	});

	it("handles WorkOS browser OAuth login, transparent cookie refresh, org switching, creation, invites, and CLI PKCE handoff", async () => {
		const testEnv = makeWorkOSEnv();
		const nowSec = Math.floor(Date.now() / 1000);

		const shortLivedJwt = await signJwt({
			sub: "user_ramon",
			email: "ramon@echophase.test",
			org_id: "org_echo",
			role: "admin",
			sid: "sess_1",
			exp: nowSec + 30, // within 60s refresh margin to test transparent refresh!
		});
		const freshJwt = await signJwt({
			sub: "user_ramon",
			email: "ramon@echophase.test",
			org_id: "org_echo",
			role: "admin",
			sid: "sess_1",
			exp: nowSec + 3600,
		});
		const secondOrgJwt = await signJwt({
			sub: "user_ramon",
			email: "ramon@echophase.test",
			org_id: "org_beta",
			role: "admin",
			sid: "sess_1",
			exp: nowSec + 3600,
		});

		const authQueue: Array<() => Response> = [
			// 1. /auth/callback exchange
			() =>
				Response.json({
					user: {
						id: "user_ramon",
						email: "ramon@echophase.test",
						first_name: "Ramon",
						last_name: "G",
					},
					organization_id: "org_echo",
					access_token: shortLivedJwt,
					refresh_token: "rt_1",
				}),
			// 2. transparent cookie refresh on /auth/me
			() =>
				Response.json({
					user: {
						id: "user_ramon",
						email: "ramon@echophase.test",
						first_name: "Ramon",
						last_name: "G",
					},
					organization_id: "org_echo",
					access_token: freshJwt,
					refresh_token: "rt_2",
				}),
			// 3. /auth/org/create refresh into org_beta
			() =>
				Response.json({
					user: {
						id: "user_ramon",
						email: "ramon@echophase.test",
						first_name: "Ramon",
						last_name: "G",
					},
					organization_id: "org_beta",
					access_token: secondOrgJwt,
					refresh_token: "rt_3",
				}),
			// 4. CLI /auth/callback exchange
			() =>
				Response.json({
					user: {
						id: "user_ramon",
						email: "ramon@echophase.test",
						first_name: "Ramon",
						last_name: "G",
					},
					organization_id: "org_echo",
					access_token: freshJwt,
					refresh_token: "rt_cli_1",
				}),
		];

		mockHandlers.push((url, init) => {
			const method = (init?.method ?? "GET").toUpperCase();
			if (url.pathname === "/user_management/authenticate" && method === "POST") {
				const fn = authQueue.shift();
				return fn
					? fn()
					: Response.json({ message: "unexpected authenticate call" }, { status: 500 });
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "GET") {
				return Response.json({
					data: [
						{
							id: "om_1",
							user_id: "user_ramon",
							organization_id: "org_echo",
							organization_name: "Echophase",
							status: "active",
							role: { slug: "admin" },
						},
					],
				});
			}
			if (url.pathname === "/organizations" && method === "POST") {
				return Response.json({ id: "org_beta", name: "Beta Labs" }, { status: 201 });
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "POST") {
				return Response.json(
					{
						id: "om_2",
						user_id: "user_ramon",
						organization_id: "org_beta",
						status: "active",
						role: { slug: "admin" },
					},
					{ status: 201 },
				);
			}
			if (url.pathname === "/user_management/invitations" && method === "POST") {
				return Response.json(
					{
						id: "inv_1",
						email: "teammate@beta.test",
						state: "pending",
						organization_id: "org_beta",
						role_slug: "member",
						expires_at: "2026-12-01T00:00:00Z",
					},
					{ status: 201 },
				);
			}
			if (url.pathname === "/user_management/invitations/inv_other_org" && method === "GET") {
				return Response.json({
					id: "inv_other_org",
					email: "victim@other.test",
					state: "pending",
					organization_id: "org_other",
					role_slug: "member",
				});
			}
			return null;
		});

		// /auth/config advertises WorkOS
		const cfgRes = await callWorker(testEnv, "https://controller.test/auth/config");
		expect(cfgRes.status).toBe(200);
		const cfg = (await cfgRes.json()) as {
			workosEnabled: boolean;
			authProvider: string;
			clientId: string;
		};
		expect(cfg.workosEnabled).toBe(true);
		expect(cfg.authProvider).toBe("authkit");
		expect(cfg.clientId).toBe(WORKOS_CLIENT_ID);

		// Step 1: GET /auth/login redirects to WorkOS AuthKit with PKCE + state cookie
		const loginStart = await callWorker(
			testEnv,
			"https://controller.test/auth/login?return_to=/questions",
			{ redirect: "manual" },
		);
		expect(loginStart.status).toBe(302);
		const authUrl = new URL(loginStart.headers.get("Location")!);
		expect(authUrl.origin).toBe(WORKOS_ORIGIN);
		expect(authUrl.pathname).toBe("/user_management/authorize");
		expect(authUrl.searchParams.get("provider")).toBe("authkit");
		expect(authUrl.searchParams.get("code_challenge_method")).toBe("S256");
		const state = authUrl.searchParams.get("state")!;
		const stateCookie = cookiePair(extractCookies(loginStart)[0]);

		// Step 2: GET /auth/callback exchanges code and sets encrypted hotbed_wos_session cookie
		const cbRes = await callWorker(
			testEnv,
			`https://controller.test/auth/callback?code=wos_code_1&state=${encodeURIComponent(state)}`,
			{ headers: { Cookie: stateCookie }, redirect: "manual" },
		);
		expect(cbRes.status).toBe(303);
		expect(cbRes.headers.get("Location")).toBe("/questions");
		const wosCookieRaw = extractCookies(cbRes).find((c) => c.startsWith("hotbed_wos_session="));
		expect(wosCookieRaw).toBeTruthy();
		const sessionCookie = cookiePair(wosCookieRaw!);

		// Step 3: Next request with near-expiry JWT transparently refreshes and emits rotated Set-Cookie
		const meRes = await callWorker(testEnv, "https://controller.test/auth/me", {
			headers: { Cookie: sessionCookie },
		});
		expect(meRes.status).toBe(200);
		const rotatedCookie = extractCookies(meRes).find((c) => c.startsWith("hotbed_wos_session="));
		expect(rotatedCookie).toBeTruthy();
		const me = (await meRes.json()) as {
			authenticated: boolean;
			user: { email: string };
			organization: { id: string; name: string; role: string };
		};
		expect(me.authenticated).toBe(true);
		expect(me.user.email).toBe("ramon@echophase.test");
		expect(me.organization).toEqual({ id: "org_echo", name: "Echophase", role: "admin" });

		// Step 4: Create a second organization via POST /auth/org/create
		const createOrgRes = await callWorker(testEnv, "https://controller.test/auth/org/create", {
			method: "POST",
			headers: {
				Cookie: cookiePair(rotatedCookie!),
				"Content-Type": "application/json",
				Accept: "application/json",
				Origin: "https://controller.test",
			},
			body: JSON.stringify({ name: "Beta Labs" }),
		});
		expect(createOrgRes.status).toBe(201);
		const createdOrg = (await createOrgRes.json()) as {
			organization: { id: string; name: string };
		};
		expect(createdOrg.organization.id).toBe("org_beta");
		expect(createdOrg.organization.name).toBe("Beta Labs");
		const betaCookie = cookiePair(
			extractCookies(createOrgRes).find((c) => c.startsWith("hotbed_wos_session="))!,
		);

		// Step 5: Invite a teammate to org_beta
		const invRes = await callWorker(testEnv, "https://controller.test/auth/org/invite", {
			method: "POST",
			headers: {
				Cookie: betaCookie,
				"Content-Type": "application/json",
				Origin: "https://controller.test",
			},
			body: JSON.stringify({ email: "teammate@beta.test", role: "member" }),
		});
		expect(invRes.status).toBe(201);
		const invBody = (await invRes.json()) as { invitation: { id: string; email: string } };
		expect(invBody.invitation.email).toBe("teammate@beta.test");

		// Cannot revoke another organization's invitation
		const crossOrgRevoke = await callWorker(
			testEnv,
			"https://controller.test/auth/org/invitations/inv_other_org",
			{
				method: "DELETE",
				headers: { Cookie: betaCookie, Origin: "https://controller.test" },
			},
		);
		expect(crossOrgRevoke.status).toBe(404);

		// Step 6: CLI loopback PKCE flow rejects non-loopback redirect_uri
		const badCliRes = await callWorker(
			testEnv,
			"https://controller.test/auth/cli/authorize?redirect_uri=https://evil.test/callback&state=s1&code_challenge=abc",
		);
		expect(badCliRes.status).toBe(400);

		// Valid CLI loopback PKCE flow completes and exchanges code with verifier
		const cliVerifier = "test_cli_pkce_verifier_1234567890abcdef";
		const cliChallenge = await pkceS256(cliVerifier);
		const cliRedirect = "http://127.0.0.1:45678/callback";
		const cliStart = await callWorker(
			testEnv,
			`https://controller.test/auth/cli/authorize?redirect_uri=${encodeURIComponent(cliRedirect)}&state=cli_s1&code_challenge=${encodeURIComponent(cliChallenge)}`,
			{ redirect: "manual" },
		);
		expect(cliStart.status).toBe(302);
		const cliWosUrl = new URL(cliStart.headers.get("Location")!);
		const cliStateCookie = cookiePair(extractCookies(cliStart)[0]);

		const cliCb = await callWorker(
			testEnv,
			`https://controller.test/auth/callback?code=wos_cli_code&state=${encodeURIComponent(cliWosUrl.searchParams.get("state")!)}`,
			{ headers: { Cookie: cliStateCookie }, redirect: "manual" },
		);
		expect(cliCb.status).toBe(303);
		const loopbackLoc = new URL(cliCb.headers.get("Location")!);
		expect(loopbackLoc.origin).toBe("http://127.0.0.1:45678");
		expect(loopbackLoc.searchParams.get("state")).toBe("cli_s1");
		const handoffCode = loopbackLoc.searchParams.get("code")!;

		// Wrong verifier is rejected
		const badExch = await callWorker(testEnv, "https://controller.test/auth/cli/exchange", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ code: handoffCode, codeVerifier: "wrong", redirectUri: cliRedirect }),
		});
		expect(badExch.status).toBe(401);

		// Right verifier succeeds and returns tokens
		const goodExch = await callWorker(testEnv, "https://controller.test/auth/cli/exchange", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				code: handoffCode,
				codeVerifier: cliVerifier,
				redirectUri: cliRedirect,
			}),
		});
		expect(goodExch.status).toBe(200);
		const cliTokens = (await goodExch.json()) as {
			accessToken: string;
			refreshToken: string;
			organization: { id: string };
		};
		expect(cliTokens.accessToken).toBe(freshJwt);
		expect(cliTokens.refreshToken).toBe("rt_cli_1");
		expect(cliTokens.organization.id).toBe("org_echo");
	});
});
