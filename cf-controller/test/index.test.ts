import { env, runDurableObjectAlarm, SELF } from "cloudflare:test";
import {
	create,
	fromBinary,
	toBinary,
	type DescMessage,
	type MessageShape,
} from "@bufbuild/protobuf";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { createFetchClient } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import {
	BaseSchema,
	BaseState,
	CladefsCellSchema,
	ClusterService,
	CommandLogSchema,
	CommandSchema,
	CommandState,
	CreateWorkspaceRequestSchema,
	FindingDecision,
	FindingSeverity,
	FindingVerdict,
	GitAuthService,
	HeartbeatRequestSchema,
	ListFilesResponseSchema,
	NodeUsageSchema,
	PromptSchema,
	QuestionService,
	QuestionState,
	QuickResponse,
	ReleaseWorkspaceResponseSchema,
	RepoSchema,
	ResourcesSchema,
	ReviewService,
	ReviewState,
	RunCommandRequestSchema,
	ScheduledCommandSchema,
	ScheduleService,
	SessionService,
	StepSchema,
	TraceCommandChunkSchema,
	WorkspaceSchema,
	WorkspaceService,
	WorkspaceState,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import type { TunnelDownMessage, TunnelUpMessage } from "../src/tunnel.ts";

const OWNER_TOKEN = "test-owner-secret";
const JOIN_TOKEN = "test-join-secret";

function encodeBase64(bytes: Uint8Array): string {
	let bin = "";
	for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
	return btoa(bin);
}

function decodeBase64(b64: string): Uint8Array {
	if (!b64) return new Uint8Array(0);
	const bin = atob(b64);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

function makeTransport(token?: string, extraHeaders?: Record<string, string>) {
	const authInterceptor: Interceptor = (next) => async (req) => {
		if (token) req.header.set("Authorization", `Bearer ${token}`);
		for (const [k, v] of Object.entries(extraHeaders ?? {})) {
			req.header.set(k, v);
		}
		return next(req);
	};
	return createTransport({
		httpClient: createFetchClient((input, init) => SELF.fetch(input, init)),
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

function ownerClients(extraHeaders?: Record<string, string>) {
	const transport = makeTransport(OWNER_TOKEN, { "X-Hotbed-Host": "macbook", ...extraHeaders });
	return {
		cluster: createClient(ClusterService, transport),
		sessions: createClient(SessionService, transport),
		workspaces: createClient(WorkspaceService, transport),
		questions: createClient(QuestionService, transport),
		reviews: createClient(ReviewService, transport),
		schedules: createClient(ScheduleService, transport),
		gitauth: createClient(GitAuthService, transport),
	};
}

function workerClients(token: string, nodeId: string, hostname: string, addr: string) {
	const transport = makeTransport(token, {
		"X-Hotbed-Node-Id": nodeId,
		"X-Hotbed-Host": hostname,
		"X-Hotbed-Addr": addr,
	});
	return {
		cluster: createClient(ClusterService, transport),
		workspaces: createClient(WorkspaceService, transport),
	};
}

async function openMockWorkerTunnel(
	token: string,
	nodeId: string,
	hostname: string,
	addr: string,
	handlers: {
		unary?: Record<string, (reqBytes: Uint8Array) => Uint8Array>;
		stream?: Record<string, (reqBytes: Uint8Array) => Uint8Array[]>;
	},
): Promise<{ ws: WebSocket; calls: string[]; close: () => void }> {
	const res = await SELF.fetch("https://controller.test/ws/worker", {
		headers: {
			Upgrade: "websocket",
			Authorization: `Bearer ${token}`,
			"X-Hotbed-Node-Id": nodeId,
			"X-Hotbed-Host": hostname,
			"X-Hotbed-Addr": addr,
		},
	});
	expect(res.status).toBe(101);
	const ws = res.webSocket!;
	expect(ws).toBeTruthy();
	ws.accept();

	const calls: string[] = [];
	ws.addEventListener("message", (ev) => {
		const raw = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data);
		if (raw === "pong") return;
		const msg = JSON.parse(raw) as TunnelDownMessage;
		if (msg.type !== "rpc_req") return;
		calls.push(msg.procedure);
		const reqBytes = decodeBase64(msg.body);
		try {
			if (msg.stream) {
				const fn = handlers.stream?.[msg.procedure];
				if (!fn) {
					ws.send(
						JSON.stringify({
							type: "rpc_err",
							id: msg.id,
							code: "NOT_FOUND",
							message: `unhandled stream ${msg.procedure}`,
						} satisfies TunnelUpMessage),
					);
					return;
				}
				for (const chunk of fn(reqBytes)) {
					ws.send(
						JSON.stringify({
							type: "rpc_msg",
							id: msg.id,
							body: encodeBase64(chunk),
						} satisfies TunnelUpMessage),
					);
				}
				ws.send(JSON.stringify({ type: "rpc_end", id: msg.id } satisfies TunnelUpMessage));
			} else {
				const fn = handlers.unary?.[msg.procedure];
				if (!fn) {
					ws.send(
						JSON.stringify({
							type: "rpc_err",
							id: msg.id,
							code: "NOT_FOUND",
							message: `unhandled unary ${msg.procedure}`,
						} satisfies TunnelUpMessage),
					);
					return;
				}
				const respBytes = fn(reqBytes);
				ws.send(
					JSON.stringify({
						type: "rpc_msg",
						id: msg.id,
						body: encodeBase64(respBytes),
					} satisfies TunnelUpMessage),
				);
			}
		} catch (err) {
			ws.send(
				JSON.stringify({
					type: "rpc_err",
					id: msg.id,
					code: "INTERNAL",
					message: err instanceof Error ? err.message : String(err),
				} satisfies TunnelUpMessage),
			);
		}
	});

	return {
		ws,
		calls,
		close: () => {
			try {
				ws.close(1000, "test done");
			} catch {
				// ignore
			}
		},
	};
}

function encodeProto<T extends DescMessage>(schema: T, msg: MessageShape<T>): Uint8Array {
	return toBinary(schema, msg);
}

describe("Cloudflare Workers single-DO hotbed controller", () => {
	it("serves /healthz and fails closed without authentication", async () => {
		const health = await SELF.fetch("https://controller.test/healthz");
		expect(health.status).toBe(200);
		const body = (await health.json()) as { ok: boolean; clusterId: string };
		expect(body.ok).toBe(true);
		expect(body.clusterId.length).toBeGreaterThan(0);

		// Unauthenticated ConnectRPC call is rejected with 403 + ErrorInfo
		const unauthClient = createClient(ClusterService, makeTransport(undefined));
		await expect(unauthClient.listNodes({})).rejects.toThrow(/authentication required/);

		// Wrong bearer token is rejected
		const badClient = createClient(ClusterService, makeTransport("wrong-token"));
		await expect(badClient.listNodes({})).rejects.toThrow(/invalid bearer token/);

		// Unauthenticated browser GET redirects to /auth/login
		const uiRes = await SELF.fetch("https://controller.test/", {
			headers: { Accept: "text/html" },
			redirect: "manual",
		});
		expect(uiRes.status).toBe(302);
		expect(uiRes.headers.get("Location")).toBe("https://controller.test/auth/login");

		// Login flow sets HttpOnly cookie and grants cockpit access
		const loginRes = await SELF.fetch("https://controller.test/auth/login", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ token: OWNER_TOKEN }),
			redirect: "manual",
		});
		expect(loginRes.status).toBe(303);
		const setCookie = loginRes.headers.get("Set-Cookie") ?? "";
		expect(setCookie).toContain("hotbed_session=");
		expect(setCookie).toContain("HttpOnly");
		const cookieVal = setCookie.split(";")[0];

		const authedUi = await SELF.fetch("https://controller.test/", {
			headers: { Cookie: cookieVal },
		});
		expect(authedUi.status).toBe(200);

		// Cross-site CSRF attempt with cookie is refused
		const csrfRes = await SELF.fetch("https://controller.test/auth/check", {
			headers: { Cookie: cookieVal, "Sec-Fetch-Site": "cross-site" },
		});
		expect(csrfRes.status).toBe(401);
	});

	it("handles full cluster, repo, project, worker join/heartbeat, and snapshot lifecycle", async () => {
		const owner = ownerClients();

		// Mint per-worker token
		const mintRes = await SELF.fetch("https://controller.test/admin/worker-token", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ nodeId: "node-ubu1", hostname: "ubu1" }),
		});
		expect(mintRes.status).toBe(201);
		const { token: workerToken } = (await mintRes.json()) as { token: string };
		expect(workerToken.startsWith("hbw_")).toBe(true);

		// Worker joins with JOIN_TOKEN
		const joiner = workerClients(JOIN_TOKEN, "node-ubu1", "ubu1", "100.73.156.10:7121");
		const joined = await joiner.cluster.join({});
		expect(joined.clusterId.length).toBeGreaterThan(0);

		// Local-only param fields are rejected by noLocalFieldsInterceptor
		await expect(
			owner.cluster.putRepo({
				id: "bad-local-field",
				url: "https://github.com/semistrict/hotbed.git",
			}),
		).rejects.toThrow(/is the client's alone \(local\)/);

		// Register repo & project + step
		const repo = await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "hotbed",
				url: "https://github.com/semistrict/hotbed.git",
				branch: "main",
			}),
		});
		expect(repo.id).toBe("hotbed");

		const proj = await owner.cluster.getProject({ name: "hotbed" });
		expect(proj.name).toBe("hotbed");
		expect(proj.repos).toContain("github.com/semistrict/hotbed");

		const updatedProj = await owner.cluster.putStep({
			project: "hotbed",
			step: create(StepSchema, {
				name: "deps",
				script: "go mod download",
			}),
		});
		expect(updatedProj.steps.length).toBe(1);

		// Heartbeat with per-worker token returns hosted projects
		const worker = workerClients(workerToken, "node-ubu1", "ubu1", "100.73.156.10:7121");
		// Worker token cannot invoke owner RPCs
		await expect(
			worker.cluster.putRepo({
				repo: create(RepoSchema, { id: "evil", url: "https://github.com/example/evil.git" }),
			}),
		).rejects.toThrow(/worker token is not authorized/);
		const hb = await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					version: "build 42",
				}),
				usage: create(NodeUsageSchema, {
					cpuBusy: 0.05,
					memoryUsedBytes: BigInt(8 * 1024 ** 3),
				}),
			}),
		);
		expect(hb.projects.length).toBe(1);
		expect(hb.projects[0].name).toBe("hotbed");

		// Snapshot shows online node + project
		const snap = await owner.cluster.getSnapshot({});
		expect(snap.nodes.length).toBe(1);
		expect(snap.nodes[0].online).toBe(true);
		expect(snap.nodes[0].hostname).toBe("ubu1");
		expect(snap.projects.some((p) => p.name === "hotbed")).toBe(true);

		// Local-only param fields are rejected by noLocalFieldsInterceptor
		await expect(owner.cluster.getSnapshot({})).resolves.toBeTruthy();
	});

	it("places workspaces and multiplexes unary + server-streaming RPCs over /ws/worker", async () => {
		const owner = ownerClients();
		const joiner = workerClients(JOIN_TOKEN, "node-ws1", "ws1", "100.73.156.11:7121");
		const joined = await joiner.cluster.join({});
		const worker = workerClients(OWNER_TOKEN, "node-ws1", "ws1", "100.73.156.11:7121");

		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "demo",
				url: "https://github.com/example/demo.git",
				branch: "main",
			}),
		});

		// Push a warm base checkpoint for demo
		await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 8,
					memoryBytes: BigInt(16 * 1024 ** 3),
				}),
				usage: create(NodeUsageSchema, {
					cpuBusy: 0.02,
					memoryUsedBytes: BigInt(4 * 1024 ** 3),
				}),
			}),
		);
		await worker.cluster.pushEvents({
			clusterId: joined.clusterId,
			epoch: "e1",
			checkpoint: {
				seq: BigInt(1),
				bases: [
					create(BaseSchema, {
						id: "base-demo-1",
						project: "demo",
						state: BaseState.WARM,
					}),
				],
				workspaces: [],
				commands: [],
			},
		});

		// Start a session
		const sess = await owner.sessions.startSession({
			project: "demo",
			t3codeThreadId: "thread-xyz",
		});
		expect(sess.project).toBe("demo");
		expect(sess.t3codeThreadId).toBe("thread-xyz");

		// Open mock worker WebSocket tunnel
		const tunnel = await openMockWorkerTunnel(
			OWNER_TOKEN,
			"node-ws1",
			"ws1",
			"100.73.156.11:7121",
			{
				unary: {
					"/hotbed.v1.WorkspaceService/CreateWorkspace": (reqBytes) => {
						const req = fromBinary(CreateWorkspaceRequestSchema, reqBytes);
						return encodeProto(
							WorkspaceSchema,
							create(WorkspaceSchema, {
								id: req.id,
								project: req.project?.name ?? "",
								sessionId: req.sessionId,
								node: "ws1",
								nodeAddr: "100.73.156.11:7121",
								state: WorkspaceState.ACTIVE,
								createdAt: BigInt(Date.now()),
							}),
						);
					},
					"/hotbed.v1.WorkspaceService/ListFiles": () =>
						encodeProto(
							ListFilesResponseSchema,
							create(ListFilesResponseSchema, {
								files: [{ path: "README.md", kind: "file", sizeBytes: BigInt(12) }],
							}),
						),
					"/hotbed.v1.WorkspaceService/GetLogLines": () =>
						encodeProto(
							CommandLogSchema,
							create(CommandLogSchema, {
								commandId: "cmd-trace-1",
								lines: ["hello from worker"],
							}),
						),
					"/hotbed.v1.WorkspaceService/ReleaseWorkspace": () =>
						encodeProto(ReleaseWorkspaceResponseSchema, create(ReleaseWorkspaceResponseSchema, {})),
				},
				stream: {
					"/hotbed.v1.WorkspaceService/TraceCommand": () => [
						encodeProto(
							TraceCommandChunkSchema,
							create(TraceCommandChunkSchema, {
								text: 'openat(AT_FDCWD, "foo", O_RDONLY) = 3',
							}),
						),
						encodeProto(
							TraceCommandChunkSchema,
							create(TraceCommandChunkSchema, {
								text: 'read(3, "bar", 3) = 3',
								done: true,
							}),
						),
					],
				},
			},
		);

		try {
			// CreateWorkspace routes through placement -> WebSocket tunnel
			const ws = await owner.workspaces.createWorkspace({
				sessionId: sess.id,
			});
			expect(ws.state).toBe(WorkspaceState.ACTIVE);
			expect(ws.node).toBe("ws1");
			expect(tunnel.calls).toContain("/hotbed.v1.WorkspaceService/CreateWorkspace");

			// Push a running command event so TraceCommand & GetLogLines know which node owns it
			const pushRes = await worker.cluster.pushEvents({
				clusterId: joined.clusterId,
				epoch: "e1",
				events: [
					{
						seq: BigInt(2),
						at: BigInt(Date.now()),
						change: {
							case: "command",
							value: {
								what: "started",
								command: create(CommandSchema, {
									id: "cmd-trace-1",
									workspaceId: ws.id,
									sessionId: sess.id,
									project: "demo",
									state: CommandState.RUNNING,
									argv: "cargo test",
								}),
							},
						},
					},
				],
			});
			expect(pushRes.checkpointNeeded).toBe(false);

			// Unary proxy through tunnel: ListFiles
			const files = await owner.workspaces.listFiles({ workspaceId: ws.id });
			expect(files.files.map((f) => f.path)).toEqual(["README.md"]);

			// Unary proxy through tunnel: GetLogLines
			const log = await owner.workspaces.getLogLines({ id: "cmd-trace-1" });
			expect(log.lines[0]).toBe("hello from worker");

			// Server-streaming proxy through tunnel: TraceCommand
			const traceLines: string[] = [];
			for await (const chunk of owner.workspaces.traceCommand({ id: "cmd-trace-1" })) {
				traceLines.push(chunk.text);
			}
			expect(traceLines).toEqual([
				'openat(AT_FDCWD, "foo", O_RDONLY) = 3',
				'read(3, "bar", 3) = 3',
			]);

			// EndSession releases workspace via tunnel
			const ended = await owner.sessions.endSession({ id: sess.id });
			expect(ended.endedAt).toBeDefined();
			expect(tunnel.calls).toContain("/hotbed.v1.WorkspaceService/ReleaseWorkspace");
		} finally {
			tunnel.close();
		}
	});

	it("handles questions, reviews, schedules (+ DO alarm), and gitauth credentials", async () => {
		const owner = ownerClients();
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "proj-q",
				url: "https://github.com/example/proj-q.git",
				branch: "main",
			}),
		});
		const sess = await owner.sessions.startSession({ project: "proj-q" });

		// Questions lifecycle
		const added = await owner.questions.addQuestions({
			sessionId: sess.id,
			questions: [
				create(PromptSchema, {
					header: "Cache",
					question: "Which cache tier should we use?",
					options: [
						{ label: "R2", description: "Zero egress object cache", implemented: true },
						{ label: "None", description: "Direct GitHub proxy" },
					],
				}),
			],
		});
		expect(added.questions.length).toBe(1);
		const qid = added.questions[0].id;

		const answered = await owner.questions.answerQuestion({
			id: qid,
			answer: { picked: ["R2"], notes: "sounds good" },
		});
		expect(answered.state).toBe(QuestionState.ANSWERED);

		const acked = await owner.questions.ackQuestions({
			sessionId: sess.id,
			ids: [qid],
		});
		expect(acked.questions[0].state).toBe(QuestionState.HANDLED);

		// Reviews lifecycle (including markdown hotbed-findings block)
		const rev = await owner.reviews.requestReview({
			sessionId: sess.id,
			title: "Check auth gate",
			instructions: "Verify fail-closed auth",
			repo: "https://github.com/example/proj-q.git",
			base: "a".repeat(40),
			head: "b".repeat(40),
		});
		expect(rev.state).toBe(ReviewState.REQUESTED);

		const started = await owner.reviews.startReview({
			id: rev.id,
			sessionId: sess.id,
			model: "gemini-4-neon",
		});
		expect(started.state).toBe(ReviewState.IN_PROGRESS);

		const addedFindings = await owner.reviews.addFindings({
			reviewId: rev.id,
			sessionId: sess.id,
			report: [
				"Looks solid overall.",
				"```hotbed-findings",
				JSON.stringify({
					findings: [
						{
							short_summary: "Missing rate limit on login",
							summary: "POST /auth/login has no backoff.",
							severity: "important",
							verdict: "CONFIRMED",
						},
					],
				}),
				"```",
			].join("\n"),
		});
		expect(addedFindings.findings.length).toBe(1);
		const fid = addedFindings.findings[0].id;
		expect(addedFindings.findings[0].severity).toBe(FindingSeverity.IMPORTANT);
		expect(addedFindings.findings[0].verdict).toBe(FindingVerdict.CONFIRMED);

		const respEv = await owner.reviews.respond({
			reviewId: rev.id,
			findingId: fid,
			quick: QuickResponse.FIX_IF_VERIFIED,
			text: "please add backoff",
		});
		expect(respEv.quick).toBe(QuickResponse.FIX_IF_VERIFIED);

		await owner.reviews.decideFindings({
			reviewId: rev.id,
			findingIds: [fid],
			decision: FindingDecision.FIXED,
			sessionId: sess.id,
			fix: { commit: "c".repeat(40), notes: "added rate limit", locations: [] },
		});
		const finished = await owner.reviews.finishReview({
			id: rev.id,
			sessionId: sess.id,
			summary: "All findings addressed",
		});
		expect(finished.state).toBe(ReviewState.DONE);

		// Schedules + DO alarm
		const sched = await owner.schedules.putSchedule({
			schedule: create(ScheduledCommandSchema, {
				name: "nightly-check",
				project: "proj-q",
				repo: "proj-q",
				cron: "0 3 * * *",
				command: create(RunCommandRequestSchema, {
					args: ["go", "test", "./..."],
				}),
			}),
		});
		expect(sched.name).toBe("nightly-check");
		expect(sched.nextAt).toBeDefined();

		const stub = env.CONTROLLER.getByName("default");
		const alarmRan = await runDurableObjectAlarm(stub);
		expect(typeof alarmRan).toBe("boolean");

		// GitAuth PAT + MintGitToken
		const cred = await owner.gitauth.putGitCredential({
			name: "gh-pat",
			host: "github.com",
			repos: ["example/proj-q"],
			token: "ghp_test_secret_token_123",
		});
		expect(cred.name).toBe("gh-pat");

		const minted = await owner.gitauth.mintGitToken({
			repo: "https://github.com/example/proj-q.git",
		});
		expect(minted.token).toBe("ghp_test_secret_token_123");
		expect(minted.username).toBe("x-access-token");
	});

	it("coordinates cladefs peer mesh, shared R2 bases, monotonic fencing tokens, runtime placement, and retired host decommission", async () => {
		const owner = ownerClients();
		const ubu1 = workerClients(OWNER_TOKEN, "node-clade-1", "clade1", "100.73.156.10:7121");
		const node2 = workerClients(OWNER_TOKEN, "node-clade-2", "clade2", "100.73.156.20:7121");
		const asahi = workerClients(OWNER_TOKEN, "node-asahi-1", "asahimbp", "100.73.156.30:7121");

		const j1 = await ubu1.cluster.join({});
		await node2.cluster.join({});
		await asahi.cluster.join({});

		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "clade-demo",
				url: "https://github.com/semistrict/clade-demo.git",
				branch: "main",
			}),
		});

		// clade1 advertises vm+chroot, cladefs peer addr, incarnation 111, and a warm shared R2 base
		const hb1 = await ubu1.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					benchCpus: "4-7",
					runtimes: ["vm", "chroot"],
				}),
				usage: create(NodeUsageSchema, { cpuBusy: 0.1, memoryUsedBytes: BigInt(4 * 1024 ** 3) }),
				cladefsPeerAddr: "100.73.156.10:9185",
				cladefsIncarnation: 111n,
				sharedBases: [
					create(CladefsCellSchema, {
						project: "clade-demo",
						arch: "amd64",
						baseId: "clade-demo-g1",
						recipe: "r1",
						tenant: 0n,
						cell: 42n,
						version: 7n,
					}),
				],
			}),
		);
		expect(hb1.cladefsPeers).toEqual(["100.73.156.10:9185"]);

		// clade2 joins peer mesh without a local base; asahimbp is chroot-only
		const hb2 = await node2.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					runtimes: ["vm", "chroot"],
				}),
				usage: create(NodeUsageSchema, { cpuBusy: 0.05, memoryUsedBytes: BigInt(2 * 1024 ** 3) }),
				cladefsPeerAddr: "100.73.156.20:9185",
				cladefsIncarnation: 222n,
			}),
		);
		expect(hb2.cladefsPeers).toEqual(["100.73.156.10:9185", "100.73.156.20:9185"]);

		await asahi.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "arm64",
					cpus: 14,
					memoryBytes: BigInt(36 * 1024 ** 3),
					runtimes: ["chroot"],
				}),
				usage: create(NodeUsageSchema, { cpuBusy: 0.01, memoryUsedBytes: BigInt(4 * 1024 ** 3) }),
			}),
		);

		const sess = await owner.sessions.startSession({ project: "clade-demo" });

		// vm runtime rejected on chroot-only asahimbp
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "asahimbp", runtime: "vm" },
			}),
		).rejects.toThrow(/does not support vm runtime/);

		// --bench + runtime=vm rejected
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "clade1", runtime: "vm", bench: true },
			}),
		).rejects.toThrow(/cannot run --bench inside vm runtime/);

		// Connect /ws/worker tunnel for clade2 to capture forwarded CreateWorkspaceRequest
		const captured: Array<MessageShape<typeof CreateWorkspaceRequestSchema>> = [];
		const tunnel = await openMockWorkerTunnel(
			OWNER_TOKEN,
			"node-clade-2",
			"clade2",
			"100.73.156.20:7121",
			{
				unary: {
					"/hotbed.v1.WorkspaceService/CreateWorkspace": (reqBytes) => {
						const req = fromBinary(CreateWorkspaceRequestSchema, reqBytes);
						captured.push(req);
						return encodeProto(
							WorkspaceSchema,
							create(WorkspaceSchema, {
								id: req.id,
								sessionId: req.sessionId,
								nodeId: "node-clade-2",
								project: "clade-demo",
								runtime: "vm",
								state: WorkspaceState.IDLE,
							}),
						);
					},
				},
			},
		);

		const created = await owner.workspaces.createWorkspace({
			sessionId: sess.id,
			repo: "clade-demo",
			placement: { node: "clade2", runtime: "vm" },
		});
		expect(created.runtime).toBe("vm");
		expect(captured.length).toBe(1);
		expect(captured[0].runtime).toBe("vm");
		expect(captured[0].sharedBase?.cell).toBe(42n);
		expect(captured[0].sharedBase?.version).toBe(7n);
		expect(captured[0].fencingToken).toBe(1n);

		// Second session gets strictly higher monotonic fencing token (2n)
		const sess2 = await owner.sessions.startSession({ project: "clade-demo" });
		await owner.workspaces.createWorkspace({
			sessionId: sess2.id,
			repo: "clade-demo",
			placement: { node: "clade2", runtime: "vm" },
		});
		expect(captured.length).toBe(2);
		expect(captured[1].fencingToken).toBe(2n);
		tunnel.close();

		// Online cladefs node cannot be retired yet
		await expect(owner.cluster.retireNode({ node: "clade1" })).rejects.toThrow(/is online/);
	});

	it("auto-runs Drizzle migrations, imports snapshots via /admin/import, and redirects workers once migrated", async () => {
		const owner = ownerClients();

		// Import a snapshot from an existing controller (e.g. ubu1) with force=true
		const importRes = await SELF.fetch("https://controller.test/admin/import", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({
				clusterId: "imported-cluster-1",
				force: true,
				tables: {
					nodes: [
						{
							id: "node-ubu1",
							hostname: "ubu1",
							login: "ramon",
							addr: "100.73.156.10:7121",
							joined_at: Date.now() - 1000,
							last_seen: Date.now(),
							epoch: "",
							applied: 0,
							applied_at: 0,
							heartbeat: null,
							retired_at: null,
						},
					],
					repos: [
						{
							id: "hotbed",
							doc: JSON.stringify({
								id: "hotbed",
								url: "https://github.com/semistrict/hotbed.git",
								branch: "main",
							}),
						},
					],
					projects: [
						{
							id: 7,
							name: "hotbed",
							created_at: Date.now() - 1000,
							setup: "{}",
						},
					],
					project_repos: [{ project_id: 7, remote: "github.com/semistrict/hotbed" }],
				},
			}),
		});
		expect(importRes.status).toBe(200);
		const imported = (await importRes.json()) as {
			ok: boolean;
			clusterId: string;
			controllerNodeId: string;
			importedTables: number;
			importedRows: number;
			workerTokens: Record<string, string>;
		};
		expect(imported.ok).toBe(true);
		expect(imported.clusterId).toBe("imported-cluster-1");
		expect(imported.controllerNodeId).toBe("cf:imported-cluster-1");
		expect(imported.importedTables).toBe(4);
		expect(imported.importedRows).toBe(4);
		expect(imported.workerTokens["node-ubu1"]?.startsWith("hbw_")).toBe(true);

		// Non-force duplicate import is refused with 409 Conflict
		const dupRes = await SELF.fetch("https://controller.test/admin/import", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ clusterId: "other", tables: {} }),
		});
		expect(dupRes.status).toBe(409);

		// Imported node can authenticate using its freshly minted per-worker token
		const worker = workerClients(
			imported.workerTokens["node-ubu1"],
			"node-ubu1",
			"ubu1",
			"100.73.156.10:7121",
		);
		const hb1 = await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, { clusterId: "imported-cluster-1" }),
		);
		expect(hb1.migratedTo).toBeUndefined();
		expect(hb1.projects.some((p) => p.name === "hotbed")).toBe(true);

		// Transition controller into migrated state (skipExport: true)
		const mig = await owner.cluster.migrateController({
			targetController: "https://next-controller.example.workers.dev",
			workerToken: "next-worker-token",
			skipExport: true,
		});
		expect(mig.targetController).toBe("https://next-controller.example.workers.dev");

		// /healthz reports migratedTo
		const health = (await (await SELF.fetch("https://controller.test/healthz")).json()) as {
			migratedTo?: string;
		};
		expect(health.migratedTo).toBe("https://next-controller.example.workers.dev");

		// Worker Heartbeat & PushEvents now receive migratedTo redirect instructions
		const hb2 = await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, { clusterId: "imported-cluster-1" }),
		);
		expect(hb2.migratedTo?.controller).toBe("https://next-controller.example.workers.dev");
		expect(hb2.migratedTo?.token).toBe("next-worker-token");
		expect(hb2.migratedTo?.controllerNodeId).toBe("cf:imported-cluster-1");

		const push2 = await worker.cluster.pushEvents({
			clusterId: "imported-cluster-1",
			epoch: "e1",
		});
		expect(push2.migratedTo?.controller).toBe("https://next-controller.example.workers.dev");

		// Mutating RPCs are refused once migrated
		await expect(owner.sessions.startSession({ project: "hotbed" })).rejects.toThrow(
			/this controller has migrated to https:\/\/next-controller\.example\.workers\.dev/,
		);
	});
});
