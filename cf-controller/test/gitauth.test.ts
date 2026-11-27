import { env, runInDurableObject, SELF } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import type { ControllerEnv } from "../src/controller-do.ts";
import { GitAuthService, GitCredentialSchema } from "../src/gen/hotbed/v1/hotbed_pb.ts";
import type { ControllerStore } from "../src/store/index.ts";

const OAUTH = "https://mock-github-oauth.test";

function gitauthClient() {
	const auth: Interceptor = (next) => async (req) => {
		req.header.set("Authorization", "Bearer test-owner-secret");
		return next(req);
	};
	return createClient(
		GitAuthService,
		createTransport({
			httpClient: createFetchClient((input, init) => SELF.fetch(input, init)),
			baseUrl: "https://controller.test",
			useBinaryFormat: true,
			interceptors: [auth],
			sendCompression: null,
			acceptCompression: [],
			compressMinBytes: 0,
			readMaxBytes: 0xffffffff,
			writeMaxBytes: 0xffffffff,
		}),
	);
}

describe("git token refresh", () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it("refreshes on each request, and one that loses the race for a single-use refresh token uses the winner's", async () => {
		const controller = (env as unknown as ControllerEnv).CONTROLLER.getByName("default");
		const inController = <T>(fn: (store: ControllerStore) => T) =>
			runInDurableObject(controller, (instance) =>
				fn((instance as unknown as { store: ControllerStore }).store),
			);
		// Read in the controller's context: the OAuth mock runs there.
		const tokenIn = (store: ControllerStore) =>
			store.gitCredentials.byHost("github.com").find((c) => c.cred.name === "gh-oauth-race")?.secret
				.token;
		const store = await inController((store) => {
			store.gitCredentials.put(
				create(GitCredentialSchema, {
					name: "gh-oauth-race",
					host: "github.com",
					repos: ["race/repo"],
					oauthUrl: OAUTH,
					refreshable: true,
				}),
				{ token: "ghu_old", refreshToken: "ghr_old", tokenExpiresAt: Date.now() - 1000 },
			);
			return store;
		});

		// Both requests are in flight before either answers. GitHub honors the
		// refresh token once: the first gets new tokens, the second an error,
		// sent once the first's tokens are stored.
		const refreshes: string[] = [];
		let bothArrived!: () => void;
		const arrived = new Promise<void>((r) => (bothArrived = r));
		const origFetch = globalThis.fetch;
		let answered = 0;
		vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
			const url = new URL(
				typeof input === "string" ? input : input instanceof URL ? input : input.url,
			);
			if (url.origin !== OAUTH) return origFetch(input, init);
			refreshes.push(
				new URLSearchParams(typeof init?.body === "string" ? init.body : "").get("refresh_token") ??
					"",
			);
			if (refreshes.length === 2) bothArrived();
			await arrived;
			if (answered++ === 0) {
				return Response.json({
					access_token: "ghu_new",
					refresh_token: "ghr_new",
					expires_in: 28800,
				});
			}
			for (let i = 0; i < 1000 && tokenIn(store) !== "ghu_new"; i++) {
				await new Promise((r) => setTimeout(r, 0));
			}
			return Response.json({ error: "bad_refresh_token", error_description: "already used" });
		});

		const client = gitauthClient();
		const [a, b] = await Promise.all([
			client.mintGitToken({ repo: "https://github.com/race/repo.git" }),
			client.mintGitToken({ repo: "https://github.com/race/repo.git" }),
		]);
		expect(refreshes).toEqual(["ghr_old", "ghr_old"]);
		expect([a.token, b.token]).toEqual(["ghu_new", "ghu_new"]);
		expect(await inController(tokenIn)).toBe("ghu_new");

		// A token still fresh is used as stored, with no refresh.
		const c = await client.mintGitToken({ repo: "https://github.com/race/repo.git" });
		expect(c.token).toBe("ghu_new");
		expect(refreshes).toHaveLength(2);
	});
});
