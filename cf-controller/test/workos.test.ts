import { env } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
	CladefsCellSchema,
	ClusterService,
	GitAuthService,
	HeartbeatRequestSchema,
	NodeUsageSchema,
	OrgControllerConfigSchema,
	RepoSchema,
	ResourcesSchema,
	SessionService,
	WorkerCladefsFUSEConfigSchema,
	WorkerConfigSchema,
	WorkerFreshenConfigSchema,
	WorkerVMConfigSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import worker from "../src/index.ts";
import type { ControllerEnv } from "../src/controller-do.ts";
import {
	base64UrlEncode,
	pkceS256,
	resetWorkOSJwksCache,
	resetWorkOSRefreshCache,
	type WorkOSEnv,
} from "../src/workos.ts";

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
		WORKOS_AUTH_PROVIDER: "GoogleOAuth",
		WORKOS_API_BASE: WORKOS_ORIGIN,
		INITIAL_SIGNUP_CODE: "LC2026051",
		ALLOW_LOCAL_REPOS: "1",
		...overrides,
	};
}

function callWorker(testEnv: ControllerEnv, input: string, init?: RequestInit): Promise<Response> {
	return worker.fetch(new Request(input, init), testEnv);
}

function makeTransport(
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
	return createTransport({
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
}

function makeRpcClient(
	testEnv: ControllerEnv,
	token?: string,
	extraHeaders?: Record<string, string>,
) {
	return createClient(ClusterService, makeTransport(testEnv, token, extraHeaders));
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
		resetWorkOSRefreshCache();
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
					authentication_method: "GoogleOAuth",
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
					authentication_method: "GoogleOAuth",
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

		// /auth/config advertises Google-only WorkOS auth + signup code requirement
		const cfgRes = await callWorker(testEnv, "https://controller.test/auth/config");
		expect(cfgRes.status).toBe(200);
		const cfg = (await cfgRes.json()) as {
			workosEnabled: boolean;
			authProvider: string;
			clientId: string;
			tokenLoginEnabled: boolean;
			signupCodeRequired: boolean;
		};
		expect(cfg.workosEnabled).toBe(true);
		expect(cfg.authProvider).toBe("GoogleOAuth");
		expect(cfg.clientId).toBe(WORKOS_CLIENT_ID);
		expect(cfg.tokenLoginEnabled).toBe(false);
		expect(cfg.signupCodeRequired).toBe(true);

		// Step 1: GET /auth/login redirects straight to GoogleOAuth via WorkOS with PKCE + state cookie
		const loginStart = await callWorker(
			testEnv,
			"https://controller.test/auth/login?return_to=/questions",
			{ redirect: "manual" },
		);
		expect(loginStart.status).toBe(302);
		const authUrl = new URL(loginStart.headers.get("Location")!);
		expect(authUrl.origin).toBe(WORKOS_ORIGIN);
		expect(authUrl.pathname).toBe("/user_management/authorize");
		expect(authUrl.searchParams.get("provider")).toBe("GoogleOAuth");
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

		// CLI callback must NOT set a browser session cookie sharing the CLI's single-use refresh token
		expect(extractCookies(cliCb).some((c) => c.startsWith("hotbed_wos_session="))).toBe(false);

		// Replaying the same CLI handoff code fails (one-time redemption)
		const replayExch = await callWorker(testEnv, "https://controller.test/auth/cli/exchange", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				code: handoffCode,
				codeVerifier: cliVerifier,
				redirectUri: cliRedirect,
			}),
		});
		expect(replayExch.status).toBe(401);

		// Cross-context token confusion: presenting the CLI handoff code as hotbed_wos_session cookie fails (PKCE intact)
		const forgedCookieRes = await callWorker(testEnv, "https://controller.test/admin/api-tokens", {
			method: "POST",
			headers: {
				Cookie: `hotbed_wos_session=${handoffCode}`,
				"Content-Type": "application/json",
				Origin: "https://controller.test",
			},
			body: JSON.stringify({ name: "pkce-bypass-attempt" }),
		});
		expect(forgedCookieRes.status).toBe(403);
	});

	it("enforces org-admin RBAC on invites/members, redacts acceptInvitationUrl for non-admins, and revokes removed members' API tokens", async () => {
		const testEnv = makeWorkOSEnv();
		const nowSec = Math.floor(Date.now() / 1000);
		const orgId = "org_rbac_revoke";

		const aliceAdminJwt = await signJwt({
			sub: "user_alice",
			email: "alice@acme.test",
			org_id: orgId,
			role: "admin",
			sid: "sess_alice",
			exp: nowSec + 3600,
		});
		const bobMemberJwt = await signJwt({
			sub: "user_bob",
			email: "bob@acme.test",
			org_id: orgId,
			role: "member",
			sid: "sess_bob",
			exp: nowSec + 3600,
		});

		const mutations: string[] = [];
		mockHandlers.push((url, init) => {
			const method = (init?.method ?? "GET").toUpperCase();
			if (url.pathname === "/user_management/users/user_alice" && method === "GET") {
				return Response.json({ id: "user_alice", email: "alice@acme.test" });
			}
			if (url.pathname === "/user_management/users/user_bob" && method === "GET") {
				return Response.json({ id: "user_bob", email: "bob@acme.test" });
			}
			if (url.pathname === `/organizations/${orgId}` && method === "GET") {
				return Response.json({ id: orgId, name: "RBAC Org" });
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "GET") {
				return Response.json({
					data: [
						{
							id: "om_alice",
							user_id: "user_alice",
							organization_id: orgId,
							status: "active",
							role: { slug: "admin" },
						},
						{
							id: "om_bob",
							user_id: "user_bob",
							organization_id: orgId,
							status: "active",
							role: { slug: "member" },
						},
					],
				});
			}
			if (url.pathname === "/user_management/users" && method === "GET") {
				return Response.json({
					data: [
						{ id: "user_alice", email: "alice@acme.test" },
						{ id: "user_bob", email: "bob@acme.test" },
					],
				});
			}
			if (url.pathname === "/user_management/invitations" && method === "GET") {
				return Response.json({
					data: [
						{
							id: "inv_secret_1",
							email: "cfo@acme.test",
							state: "pending",
							organization_id: orgId,
							role_slug: "admin",
							accept_invitation_url: "https://auth.workos.test/accept/secret_token",
						},
					],
				});
			}
			if (url.pathname === "/user_management/invitations" && method === "POST") {
				mutations.push("INVITE");
				return Response.json(
					{ id: "inv_bad", email: "x@y.test", state: "pending" },
					{ status: 201 },
				);
			}
			if (url.pathname === "/user_management/invitations/inv_secret_1" && method === "GET") {
				return Response.json({
					id: "inv_secret_1",
					email: "cfo@acme.test",
					state: "pending",
					organization_id: orgId,
					role_slug: "admin",
				});
			}
			if (
				url.pathname === "/user_management/invitations/inv_secret_1/revoke" &&
				method === "POST"
			) {
				mutations.push("REVOKE");
				return new Response(null, { status: 204 });
			}
			if (
				url.pathname === "/user_management/organization_memberships/om_alice" &&
				method === "GET"
			) {
				return Response.json({
					id: "om_alice",
					user_id: "user_alice",
					organization_id: orgId,
					status: "active",
					role: { slug: "admin" },
				});
			}
			if (url.pathname === "/user_management/organization_memberships/om_bob" && method === "GET") {
				return Response.json({
					id: "om_bob",
					user_id: "user_bob",
					organization_id: orgId,
					status: "active",
					role: { slug: "member" },
				});
			}
			if (
				url.pathname.startsWith("/user_management/organization_memberships/") &&
				method === "DELETE"
			) {
				mutations.push(`DELETE:${url.pathname.split("/").pop()}`);
				return new Response(null, { status: 204 });
			}
			return null;
		});

		// Non-admin Bob cannot invite, revoke invitations, or remove Alice
		const bobInvite = await callWorker(testEnv, "https://controller.test/auth/org/invite", {
			method: "POST",
			headers: { Authorization: `Bearer ${bobMemberJwt}`, "Content-Type": "application/json" },
			body: JSON.stringify({ email: "attacker@evil.test", role: "admin" }),
		});
		expect(bobInvite.status).toBe(403);

		const bobRevokeInv = await callWorker(
			testEnv,
			"https://controller.test/auth/org/invitations/inv_secret_1",
			{ method: "DELETE", headers: { Authorization: `Bearer ${bobMemberJwt}` } },
		);
		expect(bobRevokeInv.status).toBe(403);

		const bobRemoveAlice = await callWorker(
			testEnv,
			"https://controller.test/auth/org/members/om_alice",
			{ method: "DELETE", headers: { Authorization: `Bearer ${bobMemberJwt}` } },
		);
		expect(bobRemoveAlice.status).toBe(403);
		expect(mutations).toEqual([]);

		// Non-admin Bob cannot read secret acceptInvitationUrl, while admin Alice can
		const bobMembersRes = await callWorker(testEnv, "https://controller.test/auth/org/members", {
			headers: { Authorization: `Bearer ${bobMemberJwt}` },
		});
		expect(bobMembersRes.status).toBe(200);
		const bobMembers = (await bobMembersRes.json()) as {
			invitations: Array<{ acceptInvitationUrl?: string }>;
		};
		expect(bobMembers.invitations[0]?.acceptInvitationUrl).toBeUndefined();

		const aliceMembersRes = await callWorker(testEnv, "https://controller.test/auth/org/members", {
			headers: { Authorization: `Bearer ${aliceAdminJwt}` },
		});
		const aliceMembers = (await aliceMembersRes.json()) as {
			invitations: Array<{ acceptInvitationUrl?: string }>;
		};
		expect(aliceMembers.invitations[0]?.acceptInvitationUrl).toBe(
			"https://auth.workos.test/accept/secret_token",
		);

		// Bob mints a personal API token, then Alice removes Bob from the org -> API token immediately revoked
		const mintRes = await callWorker(testEnv, "https://controller.test/admin/api-tokens", {
			method: "POST",
			headers: { Authorization: `Bearer ${bobMemberJwt}`, "Content-Type": "application/json" },
			body: JSON.stringify({ name: "bob-ci" }),
		});
		expect(mintRes.status).toBe(201);
		const bobApiToken = ((await mintRes.json()) as { token: string }).token;

		const bobApiClient = makeRpcClient(testEnv, bobApiToken);
		await expect(bobApiClient.listNodes({})).resolves.toBeTruthy();

		const removeBob = await callWorker(testEnv, "https://controller.test/auth/org/members/om_bob", {
			method: "DELETE",
			headers: { Authorization: `Bearer ${aliceAdminJwt}` },
		});
		expect(removeBob.status).toBe(200);
		expect(mutations).toEqual(["DELETE:om_bob"]);

		await expect(bobApiClient.listNodes({})).rejects.toThrow(/invalid bearer token/);
		const bobMeAfter = await callWorker(testEnv, "https://controller.test/auth/me", {
			headers: { Authorization: `Bearer ${bobApiToken}` },
		});
		expect(bobMeAfter.status).toBe(401);
	});

	it("prevents /healthz org-metadata leaks, pre-auth org_id pinning, overlapping refresh logouts, stale role/org retention, and dot OWNER_TOKEN rejection", async () => {
		const testEnv = makeWorkOSEnv({ OWNER_TOKEN: "owner.token.with.dots" });
		const nowSec = Math.floor(Date.now() / 1000);

		// 1. Forged org-scoped token must not leak private org's clusterId on /healthz or pin org_id pre-auth
		const secretOrgJwt = await signJwt({
			sub: "user_secret",
			email: "ceo@secret.test",
			org_id: "org_secret_corp",
			role: "admin",
			exp: nowSec + 3600,
		});
		const secretNodes = await makeRpcClient(testEnv, secretOrgJwt).listNodes({});

		const forgedHealth = await callWorker(testEnv, "https://controller.test/healthz", {
			headers: { Authorization: "Bearer hbu_org_secret_corp.0000000000000000deadbeef" },
		});
		const forgedHealthBody = (await forgedHealth.json()) as { clusterId: string; orgId?: string };
		expect(forgedHealthBody.clusterId).not.toBe(secretNodes.clusterId);
		expect(forgedHealthBody.orgId).not.toBe("org_secret_corp");

		const unauthedRpc = await callWorker(
			testEnv,
			"https://controller.test/hotbed.v1.ClusterService/ListNodes",
			{
				method: "POST",
				headers: {
					Authorization: "Bearer hbu_org_unauthed_spawn.0000000000000000deadbeef",
					"Content-Type": "application/json",
				},
				body: "{}",
			},
		);
		expect(unauthedRpc.status).toBe(403);
		const spawnedDoHealth = await testEnv.CONTROLLER.getByName("org:org_unauthed_spawn").fetch(
			new Request("https://controller.test/healthz"),
		);
		expect(((await spawnedDoHealth.json()) as { orgId?: string }).orgId).toBeUndefined();

		// 2. OWNER_TOKEN containing a dot works on GET /auth/me when WorkOS is enabled
		const ownerMe = await callWorker(testEnv, "https://controller.test/auth/me", {
			headers: { Authorization: "Bearer owner.token.with.dots" },
		});
		expect(ownerMe.status).toBe(200);

		// 3. Staggered overlapping browser requests, demoted role refresh, failed switch preserving rotated cookie,
		// and rejecting org-less JWT on switch
		const expiredAdminJwt = await signJwt({
			sub: "user_pat",
			email: "pat@acme.test",
			org_id: "org_refresh_test",
			role: "admin",
			sid: "sess_pat",
			exp: nowSec - 10, // already expired
		});
		const demotedJwt = await signJwt({
			sub: "user_pat",
			email: "pat@acme.test",
			org_id: "org_refresh_test",
			// role omitted -> demoted to member!
			sid: "sess_pat",
			exp: nowSec + 3600,
		});
		const orgLessJwt = await signJwt({
			sub: "user_pat",
			email: "pat@acme.test",
			sid: "sess_pat",
			exp: nowSec + 3600,
		});
		const activeRts = new Set(["rt_overlap_1", "rt_switch_1"]);
		mockHandlers.push((url, init) => {
			const method = (init?.method ?? "GET").toUpperCase();
			if (url.pathname === "/user_management/authenticate" && method === "POST") {
				const body = JSON.parse(typeof init?.body === "string" ? init.body : "{}") as {
					grant_type?: string;
					refresh_token?: string;
					organization_id?: string;
				};
				if (body.grant_type === "authorization_code") {
					return Response.json({
						user: { id: "user_pat", email: "pat@acme.test" },
						organization_id: "org_refresh_test",
						authentication_method: "GoogleOAuth",
						access_token: expiredAdminJwt,
						refresh_token: "rt_overlap_1",
					});
				}
				if (body.grant_type === "refresh_token") {
					if (body.refresh_token === "rt_overlap_1" && activeRts.has("rt_overlap_1")) {
						activeRts.delete("rt_overlap_1");
						activeRts.add("rt_overlap_2");
						return Response.json({
							user: { id: "user_pat", email: "pat@acme.test" },
							organization_id: "org_refresh_test",
							access_token: demotedJwt,
							refresh_token: "rt_overlap_2",
						});
					}
					if (body.refresh_token === "rt_overlap_2" && body.organization_id === "org_forbidden") {
						return Response.json({ message: "not a member" }, { status: 403 });
					}
					if (body.refresh_token === "rt_switch_1" && activeRts.has("rt_switch_1")) {
						activeRts.delete("rt_switch_1");
						return Response.json({
							user: { id: "user_pat", email: "pat@acme.test" },
							access_token: orgLessJwt,
							refresh_token: "rt_switch_2",
						});
					}
					return Response.json({ message: "refresh token already used" }, { status: 401 });
				}
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "GET") {
				return Response.json({ data: [] });
			}
			if (url.pathname === "/organizations/org_refresh_test" && method === "GET") {
				return Response.json({ id: "org_refresh_test", name: "Refresh Org" });
			}
			return null;
		});

		// Obtain initial session cookie via OAuth callback (with expired admin JWT + rt_overlap_1)
		const loginRes = await callWorker(testEnv, "https://controller.test/auth/login", {
			redirect: "manual",
		});
		const state = new URL(loginRes.headers.get("Location")!).searchParams.get("state")!;
		const stateCookie = cookiePair(extractCookies(loginRes)[0]);
		const cbRes = await callWorker(
			testEnv,
			`https://controller.test/auth/callback?code=c1&state=${encodeURIComponent(state)}`,
			{ headers: { Cookie: stateCookie }, redirect: "manual" },
		);
		const staleCookie = cookiePair(
			extractCookies(cbRes).find((c) => c.startsWith("hotbed_wos_session="))!,
		);

		// First request rotates rt_overlap_1 -> rt_overlap_2 (and demotes role from admin to member)
		const req1 = await callWorker(testEnv, "https://controller.test/auth/check", {
			headers: { Cookie: staleCookie },
		});
		expect(req1.status).toBe(200);
		const req1Body = (await req1.json()) as { orgRole: string };
		expect(req1Body.orgRole).toBe("member");
		const rotatedCookie = extractCookies(req1).find((c) => c.startsWith("hotbed_wos_session="));
		expect(rotatedCookie).toBeTruthy();

		// Staggered second request still carrying staleCookie succeeds via short recent-refresh cache and does NOT clear cookie
		const req2 = await callWorker(testEnv, "https://controller.test/", {
			headers: { Cookie: staleCookie },
		});
		expect(req2.status).toBe(200);
		expect(extractCookies(req2).some((c) => c.includes("Max-Age=0"))).toBe(false);

		// Failed POST /auth/org/switch after a fresh rotation still returns rotated Set-Cookie
		resetWorkOSRefreshCache();
		activeRts.add("rt_overlap_1");
		const failedSwitch = await callWorker(testEnv, "https://controller.test/auth/org/switch", {
			method: "POST",
			headers: {
				Cookie: staleCookie,
				"Content-Type": "application/json",
				Accept: "application/json",
				Origin: "https://controller.test",
			},
			body: JSON.stringify({ organizationId: "org_forbidden" }),
		});
		expect(failedSwitch.status).toBe(401);
		expect(extractCookies(failedSwitch).some((c) => c.startsWith("hotbed_wos_session="))).toBe(
			true,
		);

		// POST /auth/org/switch rejects an org-less JWT returned by WorkOS
		const orgLessSwitch = await callWorker(testEnv, "https://controller.test/auth/org/switch", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${demotedJwt}`,
				"Content-Type": "application/json",
				Accept: "application/json",
			},
			body: JSON.stringify({
				organizationId: "org_victim_secret",
				refreshToken: "rt_switch_1",
			}),
		});
		expect(orgLessSwitch.status).toBe(401);

		// 4. GET /auth/callback?error=... forwards error to CLI loopback redirect_uri instead of hanging CLI
		const cliChallenge = await pkceS256("verifier_err_test_1234567890123456");
		const cliErrStart = await callWorker(
			testEnv,
			`https://controller.test/auth/cli/authorize?redirect_uri=${encodeURIComponent("http://127.0.0.1:45679/callback")}&state=cli_err_s1&code_challenge=${encodeURIComponent(cliChallenge)}`,
			{ redirect: "manual" },
		);
		const cliErrCookie = cookiePair(extractCookies(cliErrStart)[0]);
		const cliErrCb = await callWorker(
			testEnv,
			"https://controller.test/auth/callback?error=access_denied&error_description=user_cancelled",
			{ headers: { Cookie: cliErrCookie }, redirect: "manual" },
		);
		expect(cliErrCb.status).toBe(303);
		const cliErrLoc = new URL(cliErrCb.headers.get("Location")!);
		expect(cliErrLoc.origin).toBe("http://127.0.0.1:45679");
		expect(cliErrLoc.searchParams.get("error")).toBe("access_denied");
		expect(cliErrLoc.searchParams.get("state")).toBe("cli_err_s1");

		// 5. Malformed percent-encoding in DELETE paths returns 400 rather than uncaught 500
		const badPercent = await callWorker(
			testEnv,
			"https://controller.test/auth/org/invitations/%E0%A4%A",
			{ method: "DELETE", headers: { Authorization: `Bearer ${demotedJwt}` } },
		);
		expect(badPercent.status).toBe(400);
	});

	it("enforces Google-only auth and requires a post-Google sign-up code (initial LC2026051) for new accounts", async () => {
		const testEnv = makeWorkOSEnv({
			OWNER_TOKEN: "superadmin-owner-token",
			DEFAULT_ORG_ID: "org_default_mapped",
			INITIAL_SIGNUP_CODE: "LC2026051",
		});
		const nowSec = Math.floor(Date.now() / 1000);

		const newUserOrgLessJwt = await signJwt({
			sub: "user_new_google",
			email: "newuser@gmail.com",
			sid: "sess_new_1",
			exp: nowSec + 3600,
		});
		const cliNewUserJwt = await signJwt({
			sub: "user_cli_new",
			email: "clinew@gmail.com",
			sid: "sess_cli_new",
			exp: nowSec + 3600,
		});
		const bypassAttackerJwt = await signJwt({
			sub: "user_bypass_attempt",
			email: "bypass@gmail.com",
			sid: "sess_bypass",
			exp: nowSec + 3600,
		});
		const defaultAdminJwt = await signJwt({
			sub: "user_admin",
			email: "ramon@echophase.com",
			org_id: "org_default_mapped",
			role: "admin",
			sid: "sess_admin",
			exp: nowSec + 3600,
		});
		const otherOrgAdminJwt = await signJwt({
			sub: "user_other_admin",
			email: "admin@other.test",
			org_id: "org_other_tenant",
			role: "admin",
			sid: "sess_other",
			exp: nowSec + 3600,
		});

		let nextAuthResponse: () => Response = () =>
			Response.json({ message: "no mock set" }, { status: 500 });

		mockHandlers.push((url, init) => {
			const method = (init?.method ?? "GET").toUpperCase();
			if (url.pathname === "/user_management/authenticate" && method === "POST") {
				return nextAuthResponse();
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "GET") {
				return Response.json({ data: [] });
			}
			if (url.pathname === "/user_management/users/user_bypass_attempt" && method === "GET") {
				return Response.json({ id: "user_bypass_attempt", email: "bypass@gmail.com" });
			}
			if (url.pathname === "/organizations" && method === "POST") {
				return Response.json({ id: "org_created_new", name: "New Team" }, { status: 201 });
			}
			if (url.pathname === "/user_management/organization_memberships" && method === "POST") {
				return Response.json(
					{
						id: "om_new_1",
						user_id: "user_new_google",
						organization_id: "org_created_new",
						status: "active",
						role: { slug: "admin" },
					},
					{ status: 201 },
				);
			}
			return null;
		});

		// 1. Google-only enforcement on login & CLI authorize endpoints
		// GET /auth/login?mode=token still redirects straight to GoogleOAuth (no token form!)
		const tokenModeGet = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/login?mode=token",
			{ redirect: "manual" },
		);
		expect(tokenModeGet.status).toBe(302);
		const googleRedirect = new URL(tokenModeGet.headers.get("Location")!);
		expect(googleRedirect.searchParams.get("provider")).toBe("GoogleOAuth");
		expect(googleRedirect.searchParams.get("redirect_uri")).toBe(
			"https://hotbed.semistrict.com/auth/callback",
		);

		// POST /auth/login is disabled when Google/WorkOS auth is enabled
		const tokenModePost = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/login", {
			method: "POST",
			headers: {
				"Content-Type": "application/x-www-form-urlencoded",
				Origin: "https://hotbed.semistrict.com",
			},
			body: "token=superadmin-owner-token",
		});
		expect(tokenModePost.status).toBe(403);

		// Non-Google provider overrides are rejected
		const nonGoogleLogin = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/login?provider=GitHubOAuth",
		);
		expect(nonGoogleLogin.status).toBe(400);

		const cliChallenge = await pkceS256("verifier_google_only_test_1234567890");
		const nonGoogleCli = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/cli/authorize?redirect_uri=${encodeURIComponent("http://127.0.0.1:45680/callback")}&state=s1&code_challenge=${encodeURIComponent(cliChallenge)}&provider=Password`,
		);
		expect(nonGoogleCli.status).toBe(400);

		// Callback rejects non-Google authentication_method (e.g. Password) and error page has no token form
		const badMethodStart = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/login", {
			redirect: "manual",
		});
		const badMethodState = new URL(badMethodStart.headers.get("Location")!).searchParams.get(
			"state",
		)!;
		const badMethodCookie = cookiePair(extractCookies(badMethodStart)[0]);
		nextAuthResponse = () =>
			Response.json({
				user: { id: "user_pw", email: "pw@example.com" },
				authentication_method: "Password",
				access_token: newUserOrgLessJwt,
				refresh_token: "rt_pw",
			});
		const badMethodCb = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/callback?code=c_pw&state=${encodeURIComponent(badMethodState)}`,
			{ headers: { Cookie: badMethodCookie }, redirect: "manual" },
		);
		expect(badMethodCb.status).toBe(401);
		const badMethodHtml = await badMethodCb.text();
		expect(badMethodHtml).toContain("Google sign-in is required");
		expect(badMethodHtml).not.toContain('name="token"');
		expect(extractCookies(badMethodCb).some((c) => c.startsWith("hotbed_wos_session="))).toBe(
			false,
		);

		// 2. New Google user flow: Google auth FIRST, then sign-up code prompt!
		const startRes = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/login?return_to=/sessions",
			{ redirect: "manual" },
		);
		expect(startRes.status).toBe(302);
		const startLoc = new URL(startRes.headers.get("Location")!);
		expect(startLoc.searchParams.get("provider")).toBe("GoogleOAuth");
		const stateCookie = cookiePair(extractCookies(startRes)[0]);

		nextAuthResponse = () =>
			Response.json({
				user: { id: "user_new_google", email: "newuser@gmail.com" },
				authentication_method: "GoogleOAuth",
				access_token: newUserOrgLessJwt,
				refresh_token: "rt_new_1",
			});
		const cbRes = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/callback?code=c_new&state=${encodeURIComponent(startLoc.searchParams.get("state")!)}`,
			{ headers: { Cookie: stateCookie }, redirect: "manual" },
		);
		expect(cbRes.status).toBe(303);
		expect(cbRes.headers.get("Location")).toBe("https://hotbed.semistrict.com/auth/signup-code");
		// Must NOT issue session cookie before sign-up code is redeemed
		expect(extractCookies(cbRes).some((c) => c.startsWith("hotbed_wos_session="))).toBe(false);
		const pendingSignupRaw = extractCookies(cbRes).find((c) =>
			c.startsWith("hotbed_pending_signup="),
		);
		expect(pendingSignupRaw).toBeTruthy();
		const pendingSignupCookie = cookiePair(pendingSignupRaw!);

		// Domain separation: pending-signup cookie cannot be forged as hotbed_wos_session
		const pendingVal = pendingSignupCookie.slice("hotbed_pending_signup=".length);
		const forgedRes = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/me", {
			headers: { Cookie: `hotbed_wos_session=${pendingVal}` },
		});
		expect(forgedRes.status).toBe(401);

		// GET /auth/signup-code renders the form with the Google user's email
		const codePage = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/signup-code", {
			headers: { Cookie: pendingSignupCookie },
		});
		expect(codePage.status).toBe(200);
		const codePageHtml = await codePage.text();
		expect(codePageHtml).toContain("newuser@gmail.com");
		expect(codePageHtml).toContain('action="/auth/signup-code"');

		// Wrong code is rejected with 403 and does NOT clear pending cookie
		const wrongCodeRes = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/signup-code",
			{
				method: "POST",
				headers: {
					Cookie: pendingSignupCookie,
					"Content-Type": "application/x-www-form-urlencoded",
					Origin: "https://hotbed.semistrict.com",
				},
				body: "code=WRONG-CODE",
				redirect: "manual",
			},
		);
		expect(wrongCodeRes.status).toBe(403);
		expect(await wrongCodeRes.text()).toContain("Invalid sign-up code");

		// Initial sign-up code LC2026051 (case-insensitive, trimmed) succeeds!
		const okCodeRes = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/signup-code", {
			method: "POST",
			headers: {
				Cookie: pendingSignupCookie,
				"Content-Type": "application/x-www-form-urlencoded",
				Origin: "https://hotbed.semistrict.com",
			},
			body: "code=+lc2026051+",
			redirect: "manual",
		});
		expect(okCodeRes.status).toBe(303);
		expect(okCodeRes.headers.get("Location")).toBe(
			"https://hotbed.semistrict.com/auth/orgs?return_to=%2Fsessions",
		);
		const sessionCookieRaw = extractCookies(okCodeRes).find((c) =>
			c.startsWith("hotbed_wos_session="),
		);
		expect(sessionCookieRaw).toBeTruthy();

		// Logging in a second time as the now-approved user skips /auth/signup-code even before org creation
		const secondStart = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/login", {
			redirect: "manual",
		});
		const secondState = new URL(secondStart.headers.get("Location")!).searchParams.get("state")!;
		const secondStateCookie = cookiePair(extractCookies(secondStart)[0]);
		const secondCb = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/callback?code=c_new_2&state=${encodeURIComponent(secondState)}`,
			{ headers: { Cookie: secondStateCookie }, redirect: "manual" },
		);
		expect(secondCb.status).toBe(303);
		expect(secondCb.headers.get("Location")).toBe("https://hotbed.semistrict.com/auth/orgs");

		// 3. Direct API bypass attempt: unapproved org-less JWT cannot call POST /auth/org/create without a sign-up code
		const bypassOrgRes = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/org/create",
			{
				method: "POST",
				headers: {
					Authorization: `Bearer ${bypassAttackerJwt}`,
					"Content-Type": "application/json",
					Accept: "application/json",
				},
				body: JSON.stringify({ name: "Bypass Org" }),
			},
		);
		expect(bypassOrgRes.status).toBe(403);

		// 4. New Google user via CLI loopback flow hits /auth/signup-code first, then redirects to 127.0.0.1 after code entry
		const cliVerifier = "cli_new_user_verifier_1234567890abcdef";
		const cliChallenge2 = await pkceS256(cliVerifier);
		const cliRedirect = "http://127.0.0.1:45681/callback";
		const cliStart = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/cli/authorize?redirect_uri=${encodeURIComponent(cliRedirect)}&state=cli_new_s1&code_challenge=${encodeURIComponent(cliChallenge2)}`,
			{ redirect: "manual" },
		);
		expect(cliStart.status).toBe(302);
		const cliStartState = new URL(cliStart.headers.get("Location")!).searchParams.get("state")!;
		const cliStartCookie = cookiePair(extractCookies(cliStart)[0]);
		nextAuthResponse = () =>
			Response.json({
				user: { id: "user_cli_new", email: "clinew@gmail.com" },
				oauth_tokens: { provider: "GoogleOAuth" },
				access_token: cliNewUserJwt,
				refresh_token: "rt_cli_new",
			});
		const cliCb = await callWorker(
			testEnv,
			`https://hotbed.semistrict.com/auth/callback?code=c_cli_new&state=${encodeURIComponent(cliStartState)}`,
			{ headers: { Cookie: cliStartCookie }, redirect: "manual" },
		);
		expect(cliCb.status).toBe(303);
		expect(cliCb.headers.get("Location")).toBe("https://hotbed.semistrict.com/auth/signup-code");
		const cliPendingCookie = cookiePair(
			extractCookies(cliCb).find((c) => c.startsWith("hotbed_pending_signup="))!,
		);
		const cliRedeem = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/signup-code", {
			method: "POST",
			headers: {
				Cookie: cliPendingCookie,
				"Content-Type": "application/x-www-form-urlencoded",
				Origin: "https://hotbed.semistrict.com",
			},
			body: "code=LC2026051",
			redirect: "manual",
		});
		expect(cliRedeem.status).toBe(303);
		const cliLoopbackLoc = new URL(cliRedeem.headers.get("Location")!);
		expect(cliLoopbackLoc.origin).toBe("http://127.0.0.1:45681");
		expect(cliLoopbackLoc.searchParams.get("state")).toBe("cli_new_s1");
		expect(cliLoopbackLoc.searchParams.get("code")).toBeTruthy();

		// 5. Admin management of sign-up codes: non-default org admin (even spoofing internal header) is forbidden; default org admin can rotate & revoke
		const otherAdminList = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/admin/signup-codes",
			{
				headers: {
					Authorization: `Bearer ${otherOrgAdminJwt}`,
					"X-Hotbed-Internal-Global-Admin": "1",
				},
			},
		);
		expect(otherAdminList.status).toBe(403);

		const listRes = await callWorker(testEnv, "https://hotbed.semistrict.com/admin/signup-codes", {
			headers: { Authorization: `Bearer ${defaultAdminJwt}` },
		});
		expect(listRes.status).toBe(200);
		const listed = (await listRes.json()) as {
			codes: Array<{ code: string; useCount: number; revokedAt: number | null }>;
			approvedUsersCount: number;
		};
		expect(listed.codes.find((c) => c.code === "LC2026051")?.useCount).toBe(2);
		expect(listed.approvedUsersCount).toBe(2);

		// Add a new code and revoke LC2026051
		const addCodeRes = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/admin/signup-codes",
			{
				method: "POST",
				headers: {
					Authorization: `Bearer ${defaultAdminJwt}`,
					"Content-Type": "application/json",
				},
				body: JSON.stringify({ code: "NEXTCODE2026", label: "batch-2", maxUses: 1 }),
			},
		);
		expect(addCodeRes.status).toBe(201);

		const revokeRes = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/admin/signup-codes/LC2026051",
			{
				method: "DELETE",
				headers: { Authorization: `Bearer ${defaultAdminJwt}` },
			},
		);
		expect(revokeRes.status).toBe(200);

		// Revoked initial code is rejected and is NOT resurrected even if DO restarts
		const revokedAttempt = await callWorker(
			testEnv,
			"https://hotbed.semistrict.com/auth/org/create",
			{
				method: "POST",
				headers: {
					Authorization: `Bearer ${bypassAttackerJwt}`,
					"Content-Type": "application/json",
					Accept: "application/json",
				},
				body: JSON.stringify({ name: "Should Fail", signupCode: "LC2026051" }),
			},
		);
		expect(revokedAttempt.status).toBe(403);

		// New code works for its 1 allowed use, then fails on a 2nd user
		const newCodeOk = await callWorker(testEnv, "https://hotbed.semistrict.com/auth/org/create", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${bypassAttackerJwt}`,
				"Content-Type": "application/json",
				Accept: "application/json",
			},
			body: JSON.stringify({ name: "Allowed With New Code", signupCode: "NEXTCODE2026" }),
		});
		expect(newCodeOk.status).toBe(201);
	});

	it("assigns unique cladefs tenants per org via global TenantRegistryDO while keeping default org on tenant 0", async () => {
		const testEnv = makeWorkOSEnv({
			DEFAULT_ORG_ID: "org_default_mapped",
		});
		const nowSec = Math.floor(Date.now() / 1000);
		const mkJwt = (orgId: string, sub: string) =>
			signJwt({
				sub,
				org_id: orgId,
				role: "admin",
				iss: `https://api.workos.com/user_management/${WORKOS_CLIENT_ID}`,
				exp: nowSec + 3600,
			});

		const jwtOrgA = await mkJwt("org_cladefs_a", "user_cladefs_a");
		const jwtOrgB = await mkJwt("org_cladefs_b", "user_cladefs_b");
		const jwtDefault = await mkJwt("org_default_mapped", "user_cladefs_def");

		// 1. Even if a caller tries to spoof X-Hotbed-Internal-Default-DO,
		//    a non-default org gets a positive tenant ID (>= 1), never tenant 0.
		const rpcOrgA = makeRpcClient(testEnv, jwtOrgA, {
			"X-Hotbed-Host": "mac-a",
			"X-Hotbed-Internal-Default-DO": "1",
		});
		const cfgA0 = await rpcOrgA.getWorkerConfig({ node: "node-a1" });
		const tenantA = cfgA0.effective?.vm?.tenantId;
		expect(tenantA).toBeDefined();
		expect(tenantA! > 0n).toBe(true);
		expect(cfgA0.effective?.cladefsFuse?.tenantId).toBe(tenantA);
		expect(cfgA0.defaults?.vm?.tenantId).toBe(tenantA);

		// 2. A second non-default org receiving concurrent cold-start requests must not hit
		//    workerd's cross-request I/O restriction and all requests see the same positive tenant.
		const rpcOrgB = makeRpcClient(testEnv, jwtOrgB, { "X-Hotbed-Host": "mac-b" });
		const [cfgB0, cfgB1, cfgB2] = await Promise.all([
			rpcOrgB.getWorkerConfig({ node: "node-b1" }),
			rpcOrgB.getWorkerConfig({ node: "node-b2" }),
			rpcOrgB.getWorkerConfig({ node: "node-b3" }),
		]);
		const tenantB = cfgB0.effective?.vm?.tenantId;
		expect(tenantB).toBeDefined();
		expect(tenantB! > 0n).toBe(true);
		expect(tenantB).not.toBe(tenantA);
		expect(cfgB0.effective?.cladefsFuse?.tenantId).toBe(tenantB);
		expect(cfgB1.effective?.vm?.tenantId).toBe(tenantB);
		expect(cfgB2.effective?.vm?.tenantId).toBe(tenantB);

		// 3. The existing/default org stays on tenant 0 (preserving existing R2 cells/data).
		const rpcDefault = makeRpcClient(testEnv, jwtDefault, { "X-Hotbed-Host": "mac-def" });
		await rpcDefault.listNodes({});

		const defHealth = await callWorker(testEnv, "https://controller.test/healthz");
		expect(defHealth.status).toBe(200);
		const defHealthJson = (await defHealth.json()) as { orgId?: string; cladefsTenantId?: number };
		expect(defHealthJson.orgId).toBe("org_default_mapped");
		expect(defHealthJson.cladefsTenantId).toBe(0);

		// Default org with a VM config receives tenant 0.
		await rpcDefault.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				vm: create(WorkerVMConfigSchema, { vcpus: 4 }),
			}),
		});
		const defCfg = await rpcDefault.getWorkerConfig({ node: "ubu-def" });
		expect(defCfg.effective?.vm?.tenantId).toBe(0n);

		// 4. Attempting to override tenant_id via SetWorkerConfig is rejected for both VM and FUSE.
		await expect(
			rpcOrgA.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					vm: create(WorkerVMConfigSchema, { tenantId: 0n }),
				}),
			}),
		).rejects.toThrow(/managed per organization/);
		await expect(
			rpcOrgA.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					cladefsFuse: create(WorkerCladefsFUSEConfigSchema, { tenantId: tenantB }),
				}),
			}),
		).rejects.toThrow(/managed per organization/);
		// Setting other fields succeeds and keeps the assigned tenant.
		await rpcOrgA.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				vm: create(WorkerVMConfigSchema, { enabled: true, vcpus: 6 }),
			}),
		});
		const cfgA1 = await rpcOrgA.getWorkerConfig({ node: "node-a1" });
		expect(cfgA1.effective?.vm?.vcpus).toBe(6);
		expect(cfgA1.effective?.vm?.tenantId).toBe(tenantA);

		// 5. Worker joining Org A automatically receives Org A's tenant in HeartbeatResponse,
		//    and mismatched-tenant sharedBases advertised in Heartbeat are ignored.
		const jtRes = await callWorker(testEnv, "https://controller.test/admin/join-tokens", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${jwtOrgA}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ label: "org-a-worker" }),
		});
		expect(jtRes.status).toBe(201);
		const jt = (await jtRes.json()) as { token: string };
		const workerJoinRpc = makeRpcClient(testEnv, jt.token, {
			"X-Hotbed-Node-Id": "node-a1",
			"X-Hotbed-Host": "node-a1",
			"X-Hotbed-Addr": "100.70.0.10:7121",
		});
		const joined = await workerJoinRpc.join({});
		const workerRpc = makeRpcClient(testEnv, joined.workerToken, {
			"X-Hotbed-Node-Id": "node-a1",
			"X-Hotbed-Host": "node-a1",
			"X-Hotbed-Addr": "100.70.0.10:7121",
		});
		const hb = await workerRpc.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, { arch: "amd64", cpus: 8 }),
				usage: create(NodeUsageSchema),
				sharedBases: [
					create(CladefsCellSchema, {
						project: "wrong-tenant-proj",
						arch: "amd64",
						recipe: "r-wrong",
						baseId: "wrong-1",
						tenant: 0n,
						cell: 11n,
						version: 1n,
					}),
					create(CladefsCellSchema, {
						project: "right-tenant-proj",
						arch: "amd64",
						recipe: "r-right",
						baseId: "right-1",
						tenant: tenantA!,
						cell: 22n,
						version: 2n,
					}),
				],
			}),
		);
		expect(hb.workerConfig?.vm?.tenantId).toBe(tenantA);
		expect(hb.workerConfig?.cladefsFuse?.tenantId).toBe(tenantA);

		// 6. Verify global TenantRegistryDO state directly:
		//    - assignments are persistent and idempotent
		//    - reassigning DEFAULT_ORG_ID to another real org cannot collide on tenant 0
		const registry = testEnv.TENANT_REGISTRY!.getByName("global");
		expect(await registry.getTenant("org_default_mapped")).toBe("0");
		expect(await registry.getTenant("org_cladefs_a")).toBe(String(tenantA));
		expect(await registry.getTenant("org_cladefs_b")).toBe(String(tenantB));
		expect(await registry.assignTenant("org_cladefs_a", { isDefault: false })).toBe(
			String(tenantA),
		);
		const thirdDefault = await registry.assignTenant("org_some_other_default", { isDefault: true });
		expect(BigInt(thirdDefault) > 0n).toBe(true);
		expect(thirdDefault).not.toBe(String(tenantA));
		expect(thirdDefault).not.toBe(String(tenantB));
	});

	it("isolates all worker/freshen/controller config and git credentials per org and enforces org-admin RBAC", async () => {
		const testEnv = makeWorkOSEnv({ DEFAULT_ORG_ID: "org_settings_default" });
		const nowSec = Math.floor(Date.now() / 1000);
		const mkJwt = (orgId: string, sub: string, role: string) =>
			signJwt({
				sub,
				org_id: orgId,
				role,
				iss: `https://api.workos.com/user_management/${WORKOS_CLIENT_ID}`,
				exp: nowSec + 3600,
			});

		const adminA = await mkJwt("org_settings_a", "user_admin_a", "admin");
		const memberA = await mkJwt("org_settings_a", "user_member_a", "member");
		const adminB = await mkJwt("org_settings_b", "user_admin_b", "admin");

		const tAdminA = makeTransport(testEnv, adminA, { "X-Hotbed-Host": "mac-a" });
		const tMemberA = makeTransport(testEnv, memberA, { "X-Hotbed-Host": "mac-a-mem" });
		const tAdminB = makeTransport(testEnv, adminB, { "X-Hotbed-Host": "mac-b" });

		const clusterAdminA = createClient(ClusterService, tAdminA);
		const gitAdminA = createClient(GitAuthService, tAdminA);
		const sessAdminA = createClient(SessionService, tAdminA);

		const clusterMemberA = createClient(ClusterService, tMemberA);
		const gitMemberA = createClient(GitAuthService, tMemberA);

		const clusterAdminB = createClient(ClusterService, tAdminB);
		const gitAdminB = createClient(GitAuthService, tAdminB);
		const sessAdminB = createClient(SessionService, tAdminB);

		// Non-admin org member cannot mutate org config, upgrade system, or manage git credentials.
		await expect(
			clusterMemberA.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					freshen: create(WorkerFreshenConfigSchema, { disableOptional: true }),
				}),
			}),
		).rejects.toThrow(/only organization admins/);
		await expect(clusterMemberA.deleteWorkerConfig({ defaults: true })).rejects.toThrow(
			/only organization admins/,
		);
		await expect(clusterMemberA.upgradeSystem({})).rejects.toThrow(/only organization admins/);
		await expect(
			gitMemberA.putGitCredential({ name: "gh", host: "github.com", token: "ghp_secret" }),
		).rejects.toThrow(/only organization admins/);
		await expect(gitMemberA.deleteGitCredential({ name: "gh" })).rejects.toThrow(
			/only organization admins/,
		);

		// Org A admin configures org-wide defaults (freshen + vm idle suspend + controller session timeout) and a node override.
		const savedA = await clusterAdminA.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				defaultRuntime: "vm",
				freshen: create(WorkerFreshenConfigSchema, {
					maxDutyCycle: 0.02,
					minIntervalSeconds: 3600,
					disableOptional: true,
				}),
				vm: create(WorkerVMConfigSchema, {
					enabled: true,
					vcpus: 6,
					memMib: 8192,
					nofile: 131072n,
				}),
				controller: create(OrgControllerConfigSchema, {
					sessionIdleTimeoutSeconds: 1800,
					offlineGraceSeconds: 43200,
				}),
			}),
		});
		expect(savedA.freshen?.maxDutyCycle).toBe(0.02);
		expect(savedA.freshen?.disableOptional).toBe(true);
		expect(savedA.vm?.nofile).toBe(131072n);
		expect(savedA.controller?.sessionIdleTimeoutSeconds).toBe(1800);

		// Org-level controller policy is rejected on per-node overrides.
		await expect(
			clusterAdminA.setWorkerConfig({
				node: "ubu1",
				config: create(WorkerConfigSchema, {
					controller: create(OrgControllerConfigSchema, { sessionIdleTimeoutSeconds: 900 }),
				}),
			}),
		).rejects.toThrow(/organization defaults only/);

		// Per-node override in Org A succeeds.
		await clusterAdminA.setWorkerConfig({
			node: "ubu1",
			config: create(WorkerConfigSchema, {
				vm: create(WorkerVMConfigSchema, { memMib: 12288 }),
			}),
		});

		// Org A admin adds an org-scoped git credential.
		await gitAdminA.putGitCredential({
			name: "github-a",
			host: "github.com",
			username: "org-a-bot",
			repos: ["org-a/*"],
			token: "ghp_org_a_secret",
		});

		// Non-admin member in Org A CAN read org config and list git credentials.
		const memberView = await clusterMemberA.getWorkerConfig({ node: "ubu1" });
		expect(memberView.defaults?.freshen?.maxDutyCycle).toBe(0.02);
		expect(memberView.effective?.vm?.memMib).toBe(12288);
		expect(memberView.effective?.vm?.nofile).toBe(131072n);
		const memberCreds = await gitMemberA.listGitCredentials({});
		expect(memberCreds.credentials.map((c) => c.name)).toEqual(["github-a"]);

		// Org B is completely isolated: sees none of Org A's config, node overrides, or git credentials.
		const viewB = await clusterAdminB.getWorkerConfig({ node: "ubu1" });
		expect(viewB.defaults?.freshen).toBeUndefined();
		expect(viewB.defaults?.controller).toBeUndefined();
		expect(viewB.nodes).toHaveLength(0);
		const credsB = await gitAdminB.listGitCredentials({});
		expect(credsB.credentials).toHaveLength(0);

		// Per-org session idle timeout is honored per org (1800s in Org A, default 3600s in Org B).
		await clusterAdminA.putRepo({
			repo: create(RepoSchema, { id: "app-a", url: "https://github.com/org-a/app-a.git" }),
		});
		await clusterAdminA.addProject({ name: "proj-a", repos: ["app-a"] });
		const sessA = await sessAdminA.startSession({ agent: "agent-a", project: "proj-a" });
		const gotA = await sessAdminA.getSession({ id: sessA.id });
		expect(gotA.idleTimeoutSeconds).toBe(1800n);

		await clusterAdminB.putRepo({
			repo: create(RepoSchema, { id: "app-b", url: "https://github.com/org-b/app-b.git" }),
		});
		await clusterAdminB.addProject({ name: "proj-b", repos: ["app-b"] });
		const sessB = await sessAdminB.startSession({ agent: "agent-b", project: "proj-b" });
		const gotB = await sessAdminB.getSession({ id: sessB.id });
		expect(gotB.idleTimeoutSeconds).toBe(3600n);
	});
});
