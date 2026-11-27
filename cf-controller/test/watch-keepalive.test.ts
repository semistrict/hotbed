import { SELF } from "cloudflare:test";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import { STREAM_KEEPALIVE_MS } from "../src/context.ts";
import { ClusterService, SessionService } from "../src/gen/hotbed/v1/hotbed_pb.ts";

const OWNER_TOKEN = "test-owner-secret";

function ownerClients() {
	const auth: Interceptor = (next) => async (req) => {
		req.header.set("Authorization", `Bearer ${OWNER_TOKEN}`);
		req.header.set("X-Hotbed-Host", "macbook");
		return next(req);
	};
	const transport = createTransport({
		httpClient: createFetchClient((input, init) => SELF.fetch(input, init)),
		baseUrl: "https://controller.test",
		useBinaryFormat: true,
		interceptors: [auth],
		sendCompression: null,
		acceptCompression: [],
		compressMinBytes: 0,
		readMaxBytes: 0xffffffff,
		writeMaxBytes: 0xffffffff,
	});
	return {
		cluster: createClient(ClusterService, transport),
		sessions: createClient(SessionService, transport),
	};
}

describe("SessionService.WatchSession keepalives", () => {
	// A watch with nothing new to say sends the session again, so its client
	// can tell a quiet watch from one a controller deploy cut off without a
	// word (api.HearKeepalives; issues #20, #21).
	it(
		"sends the session again when it has sent nothing for STREAM_KEEPALIVE_MS",
		async () => {
			const owner = ownerClients();
			await owner.cluster.addProject({
				name: "quiet-watch",
				repos: ["github.com/org/quiet-watch"],
			});
			const sess = await owner.sessions.startSession({ project: "quiet-watch" });
			const controller = new AbortController();
			const sentAt: number[] = [];
			const start = Date.now();
			try {
				for await (const res of owner.sessions.watchSession(
					{ sessionId: sess.id, timeoutSeconds: 60 },
					{ signal: controller.signal },
				)) {
					expect(res.events).toEqual([]);
					expect(res.ended).toBe(false);
					sentAt.push(Date.now() - start);
					if (sentAt.length === 2) break;
				}
			} finally {
				controller.abort();
			}
			expect(sentAt.length).toBe(2);
			expect(sentAt[1] - sentAt[0]).toBeGreaterThanOrEqual(STREAM_KEEPALIVE_MS - 100);
			expect(sentAt[1] - sentAt[0]).toBeLessThan(STREAM_KEEPALIVE_MS + 2_000);
		},
		STREAM_KEEPALIVE_MS * 3,
	);
});
