import { env, runDurableObjectAlarm, runInDurableObject, SELF } from "cloudflare:test";
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
	AdmissionSchema,
	AdmissionSignalSchema,
	AppliedStepSchema,
	BaseChangeSchema,
	BaseSchema,
	BaseState,
	CheckpointSchema,
	CladefsCellSchema,
	ClusterService,
	CommandChangeSchema,
	CommandEventSchema,
	CommandLimitsSchema,
	CommandLogSchema,
	CommandSchema,
	CommandState,
	CommandUsageSchema,
	CreateWorkspaceRequestSchema,
	EventSchema,
	FindingDecision,
	FindingSeverity,
	FindingVerdict,
	FreshenPhaseSchema,
	FreshenState,
	GitAuthService,
	HeartbeatRequestSchema,
	HostUsageSchema,
	ListFilesResponseSchema,
	ManagedArtifactSchema,
	MemoryMode,
	MemoryScope,
	MemoryService,
	MemoryState,
	NodeSystemUpgradeSchema,
	NodeUsageSchema,
	PromptSchema,
	PushEventsRequestSchema,
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
	SharedBaseSchema,
	StepSchema,
	TraceCommandChunkSchema,
	WorkerConfigSchema,
	WorkerFreshenConfigSchema,
	WorkerSystemConfigSchema,
	WorkerUpgradeSystemRequestSchema,
	WorkerVMConfigSchema,
	WorkspaceSchema,
	WorkspaceService,
	WorkspaceState,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { hostStarvation, hostStarvationCost } from "../src/hostusage.ts";
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
		memories: createClient(MemoryService, transport),
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
		// A worker reads its own config (none is set here), and no other scope.
		const own = await worker.cluster.getWorkerConfig({ node: "ubu1" });
		expect(own.nodes).toEqual([]);
		await expect(worker.cluster.getWorkerConfig({ node: "asahimbp" })).rejects.toThrow(
			/a worker may read only its own config \(ubu1\), not asahimbp/,
		);
		await expect(worker.cluster.getWorkerConfig({})).rejects.toThrow(
			/a worker may read only its own config \(ubu1\), not every scope/,
		);
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
					runtimes: ["vm"],
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
		const sess = await owner.sessions.startSession({
			project: "proj-q",
			t3codeThreadId: "thread-proj-q",
		});

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
		expect(added.questions[0].t3codeThreadId).toBe("thread-proj-q");
		const qid = added.questions[0].id;

		const answered = await owner.questions.answerQuestion({
			id: qid,
			answer: { picked: ["R2"], notes: "sounds good" },
		});
		expect(answered.state).toBe(QuestionState.ANSWERED);
		expect(answered.t3codeThreadId).toBe("thread-proj-q");

		const outbox = await owner.questions.listQuestions({
			states: [QuestionState.ANSWERED, QuestionState.DISMISSED],
			t3codeThreadId: "thread-proj-q",
		});
		expect(outbox.questions.map((q) => q.id)).toEqual([qid]);
		expect(outbox.sessions).toEqual([
			expect.objectContaining({
				sessionId: sess.id,
				project: "proj-q",
				t3codeThreadId: "thread-proj-q",
				live: true,
				questionIds: [qid],
				answered: 1,
				dismissed: 0,
				open: 0,
			}),
		]);

		const acked = await owner.questions.ackQuestions({
			sessionId: sess.id,
			ids: [qid],
		});
		expect(acked.questions[0].state).toBe(QuestionState.HANDLED);

		// Reviews lifecycle (including markdown hotbed-findings block, implementer & multi-reviewer tracking)
		const rev = await owner.reviews.requestReview({
			sessionId: sess.id,
			model: "claude-opus-4-6",
			title: "Check auth gate",
			instructions: "Verify fail-closed auth",
			repo: "https://github.com/example/proj-q.git",
			base: "a".repeat(40),
			head: "b".repeat(40),
		});
		expect(rev.state).toBe(ReviewState.REQUESTED);
		expect(rev.implementerModel).toBe("claude-opus-4-6");
		expect(rev.implementer?.model).toBe("claude-opus-4-6");

		const started = await owner.reviews.startReview({
			id: rev.id,
			sessionId: sess.id,
			model: "gemini-4-neon",
			subagent: "primary-reviewer",
		});
		expect(started.state).toBe(ReviewState.IN_PROGRESS);
		expect(started.reviewers.length).toBe(1);
		expect(started.reviewers[0].model).toBe("gemini-4-neon");
		expect(started.reviewers[0].subagent).toBe("primary-reviewer");

		const addedFindings = await owner.reviews.addFindings({
			reviewId: rev.id,
			sessionId: sess.id,
			report: [
				"Looks solid overall.",
				"```json hotbed-findings",
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
		expect(addedFindings.findings[0].reviewerId).toBe(1);
		expect(addedFindings.findings[0].found?.model).toBe("gemini-4-neon");

		const respEv = await owner.reviews.respond({
			reviewId: rev.id,
			findingId: fid,
			quick: QuickResponse.FIX_IF_VERIFIED,
			text: "please add backoff",
		});
		expect(respEv.quick).toBe(QuickResponse.FIX_IF_VERIFIED);

		const beforeFix = await owner.reviews.getReview({ id: rev.id });
		expect(beforeFix.findings[0].quick).toBe(QuickResponse.FIX_IF_VERIFIED);
		expect(beforeFix.findings[0].quickBy?.session).toBe("");

		const decidedFixed = await owner.reviews.decideFindings({
			reviewId: rev.id,
			findingIds: [fid],
			decision: FindingDecision.FIXED,
			sessionId: sess.id,
			fix: { commit: "c".repeat(40), notes: "added rate limit", locations: [] },
		});
		expect(decidedFixed.reminder).toContain("hotbed memory add");
		expect(decidedFixed.findings[0].decided?.model).toBe("claude-opus-4-6");
		const revResponses = await owner.reviews.listResponses({ reviewId: rev.id, after: 0n });
		expect(revResponses.responses.length).toBe(1);
		expect(revResponses.reminder).toContain("hotbed memory add");
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

		// clade1 advertises vm, cladefs peer addr, incarnation 111, and a warm shared R2 base
		const hb1 = await ubu1.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					benchCpus: "4-7",
					runtimes: ["vm"],
				}),
				usage: create(NodeUsageSchema, { cpuBusy: 0.1, memoryUsedBytes: BigInt(4 * 1024 ** 3) }),
				cladefsPeerAddr: "100.73.156.10:9185",
				cladefsIncarnation: 111n,
				heldBases: [
					create(SharedBaseSchema, {
						cell: create(CladefsCellSchema, {
							project: "clade-demo",
							arch: "amd64",
							baseId: "clade-demo-g1",
							recipe: "r1",
							runtime: "vm",
							tenant: 0n,
							cell: 42n,
							version: 7n,
						}),
					}),
				],
			}),
		);
		expect(hb1.cladefsPeers).toEqual(["100.73.156.10:9185"]);

		// clade2 joins peer mesh without a local base; asahimbp is cladefs-only
		const hb2 = await node2.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					runtimes: ["vm"],
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
					runtimes: ["cladefs"],
				}),
				usage: create(NodeUsageSchema, { cpuBusy: 0.01, memoryUsedBytes: BigInt(4 * 1024 ** 3) }),
			}),
		);

		const sess = await owner.sessions.startSession({ project: "clade-demo" });

		// vm runtime rejected on cladefs-only asahimbp
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "asahimbp", runtime: "vm" },
			}),
		).rejects.toThrow(/does not support vm runtime/);

		// removed chroot runtime rejected upfront
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "asahimbp", runtime: "chroot" },
			}),
		).rejects.toThrow(/unknown runtime "chroot"/);

		// --bench + runtime=vm rejected
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "clade1", runtime: "vm", bench: true },
			}),
		).rejects.toThrow(/cannot run --bench inside vm runtime/);

		// --bench rejected on vm-only clade1 even without explicit runtime
		await expect(
			owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "clade1", bench: true },
			}),
		).rejects.toThrow(/does not support cladefs runtime/);

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

		try {
			const created = await owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "clade-demo",
				placement: { node: "clade2", runtime: "vm" },
			});
			expect(created.runtime).toBe("vm");
			expect(captured.length).toBe(1);
			expect(captured[0].runtime).toBe("vm");
			expect(captured[0].sharedBase?.cell?.cell).toBe(42n);
			expect(captured[0].sharedBase?.cell?.version).toBe(7n);
			expect(captured[0].fencingToken).toBeGreaterThan(0n);

			// Second session gets strictly higher monotonic fencing token
			const sess2 = await owner.sessions.startSession({ project: "clade-demo" });
			await owner.workspaces.createWorkspace({
				sessionId: sess2.id,
				repo: "clade-demo",
				placement: { node: "clade2", runtime: "vm" },
			});
			expect(captured.length).toBe(2);
			expect(captured[1].fencingToken).toBe(captured[0].fencingToken + 1n);
		} finally {
			tunnel.close();
		}

		// Online cladefs node cannot be retired yet
		await expect(owner.cluster.retireNode({ node: "clade1" })).rejects.toThrow(/is online/);
	});

	it("auto-runs Drizzle migrations on DO boot and removes retired /admin/import", async () => {
		const res = await SELF.fetch("https://controller.test/admin/import", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"Content-Type": "application/json",
			},
			body: "{}",
		});
		expect(res.status).toBe(404);
	});

	it("supports MemoryService auto-approval, full editing, promoting to global, superseding, removal, and session recall", async () => {
		const owner = ownerClients();
		await owner.cluster.addProject({
			name: "mem-proj",
			repos: ["github.com/org/mem-proj", "github.com/org/mem-extra"],
		});
		await owner.cluster.addProject({ name: "other-proj", repos: ["github.com/org/other-proj"] });
		const s1 = await owner.sessions.startSession({ project: "mem-proj" });

		// Agent saves global always-on rule + project on-demand memory — both are auto-approved immediately.
		const m1 = await owner.memories.proposeMemory({
			sessionId: s1.id,
			global: true,
			content: {
				slug: "use-neon",
				title: "Subagent model",
				summary: "Use gemini-4-neon for subagents.",
				body: "Optional detail body.",
				mode: MemoryMode.ALWAYS,
				tags: ["subagents", "models"],
			},
			rationale: "user preference",
		});
		expect(m1.state).toBe(MemoryState.APPROVED);
		expect(m1.scope).toBe(MemoryScope.GLOBAL);
		expect(m1.reminder).toBeUndefined();

		const m2 = await owner.memories.proposeMemory({
			sessionId: s1.id,
			repo: "github.com/org/mem-proj",
			content: {
				slug: "build-tips",
				title: "Build tips",
				summary: "Build tips for mem-proj",
				body: "Always run pnpm build first.",
				mode: MemoryMode.ON_DEMAND,
				tags: ["build"],
			},
			rationale: "discovered during build",
		});
		expect(m2.state).toBe(MemoryState.APPROVED);
		expect(m2.project).toBe("mem-proj");
		expect(m2.repo).toBe("github.com/org/mem-proj");

		// Snapshot reports 0 pending memories.
		const snap = await owner.cluster.getSnapshot({});
		expect(snap.pendingMemories).toBe(0);
		expect(snap.globalPendingMemories).toBe(0);

		// New session immediately receives both in briefing + memory-saving reminder.
		const s2 = await owner.sessions.startSession({ project: "mem-proj" });
		expect(s2.memories?.always.map((e) => e.slug)).toEqual(["use-neon"]);
		expect(s2.memories?.onDemand.map((e) => e.slug)).toEqual(["build-tips"]);
		expect(s2.memories?.totalApproved).toBe(2);
		expect(s2.reminder).toContain("hotbed memory add");

		// Omitting mode defaults to ALWAYS when body is empty and ON_DEMAND when body is set.
		const mAutoAlways = await owner.memories.proposeMemory({
			sessionId: s1.id,
			rationale: "testing default always mode",
			content: { slug: "auto-always-rule", summary: "Concise inline rule without --mode." },
		});
		expect(mAutoAlways.content?.mode).toBe(MemoryMode.ALWAYS);
		const mAutoOnDemand = await owner.memories.proposeMemory({
			sessionId: s1.id,
			rationale: "testing default on_demand mode",
			content: {
				slug: "auto-on-demand-playbook",
				summary: "Trigger line without --mode.",
				body: "Full playbook body.",
			},
		});
		expect(mAutoOnDemand.content?.mode).toBe(MemoryMode.ON_DEMAND);
		await expect(
			owner.memories.proposeMemory({
				sessionId: s1.id,
				rationale: "testing explicit on_demand without body",
				content: {
					slug: "explicit-on-demand-no-body",
					summary: "Explicit on_demand still requires body.",
					mode: MemoryMode.ON_DEMAND,
				},
			}),
		).rejects.toThrow(/require --body/);
		await owner.memories.deleteMemory({ id: mAutoAlways.id });
		await owner.memories.deleteMemory({ id: mAutoOnDemand.id });

		// Ending a session returns a wrap-up memory reminder.
		const sEnd = await owner.sessions.startSession({ project: "mem-proj" });
		const sEnded = await owner.sessions.endSession({ id: sEnd.id });
		expect(sEnded.reminder).toContain("hotbed memory add");

		// Editing content on a project memory without passing repo preserves its repo!
		const m2Edited = await owner.memories.reviseMemory({
			id: m2.id,
			project: "mem-proj",
			content: { summary: "Updated build tips for mem-proj" },
		});
		expect(m2Edited.repo).toBe("github.com/org/mem-proj");
		expect(m2Edited.content?.summary).toBe("Updated build tips for mem-proj");
		expect(m2Edited.content?.body).toBe("Always run pnpm build first.");

		// Editing can change repo, clear repo, edit/clear rationale, and replace/clear title, body, tags.
		const m2RepoChanged = await owner.memories.reviseMemory({
			id: m2.id,
			repo: "github.com/org/mem-extra",
			rationale: "updated rationale",
		});
		expect(m2RepoChanged.repo).toBe("github.com/org/mem-extra");
		expect(m2RepoChanged.provenance?.rationale).toBe("updated rationale");

		const m1Cleared = await owner.memories.reviseMemory({
			id: m1.id,
			replaceContent: true,
			content: {
				slug: "use-neon",
				title: "",
				summary: "Use gemini-4-neon for subagents (edited).",
				body: "",
				mode: MemoryMode.ALWAYS,
				tags: [],
			},
			rationale: "",
		});
		expect(m1Cleared.content?.title).toBe("");
		expect(m1Cleared.content?.body).toBe("");
		expect(m1Cleared.content?.tags).toEqual([]);
		expect(m1Cleared.provenance?.rationale).toBe("");

		// Promote project-specific m2 to global — clears project & repo, records "promoted", visible in other-proj!
		const m2Promoted = await owner.memories.reviseMemory({
			id: m2.id,
			global: true,
		});
		expect(m2Promoted.scope).toBe(MemoryScope.GLOBAL);
		expect(m2Promoted.project).toBe("");
		expect(m2Promoted.repo).toBe("");

		const m2Shown = await owner.memories.showMemory({ ref: String(m2.id) });
		expect(m2Shown.history.some((h) => h.what === "promoted")).toBe(true);

		// Promoting a project memory whose slug collides with an existing global memory fails with conflict,
		// and succeeds when renamed in the same edit call.
		const mCollide = await owner.memories.proposeMemory({
			sessionId: s1.id,
			rationale: "testing slug collision on promote",
			content: {
				slug: "build-tips",
				summary: "Project-specific build tips colliding with global",
				mode: MemoryMode.ALWAYS,
			},
		});
		await expect(owner.memories.reviseMemory({ id: mCollide.id, global: true })).rejects.toThrow(
			/already uses slug/,
		);
		const mCollidePromoted = await owner.memories.reviseMemory({
			id: mCollide.id,
			global: true,
			content: { slug: "build-tips-extra" },
		});
		expect(mCollidePromoted.scope).toBe(MemoryScope.GLOBAL);
		expect(mCollidePromoted.content?.slug).toBe("build-tips-extra");
		await owner.memories.deleteMemory({ id: mCollide.id });

		// Agent archiving requires a reason; archiving, editing while archived, and restoring works cleanly.
		await expect(owner.memories.archiveMemory({ id: m2.id, sessionId: s2.id })).rejects.toThrow(
			/--reason/,
		);
		const m2Arch = await owner.memories.archiveMemory({
			id: m2.id,
			sessionId: s2.id,
			reason: "temp",
		});
		expect(m2Arch.state).toBe(MemoryState.ARCHIVED);
		expect(m2Arch.archived?.reason).toBe("temp");
		const m2ArchEdited = await owner.memories.reviseMemory({
			id: m2.id,
			content: { summary: "Updated while archived" },
		});
		expect(m2ArchEdited.content?.summary).toBe("Updated while archived");
		const m2Restored = await owner.memories.restoreMemory({ id: m2.id });
		expect(m2Restored.state).toBe(MemoryState.APPROVED);

		const sOther = await owner.sessions.startSession({ project: "other-proj" });
		expect(sOther.memories?.onDemand.map((e) => e.slug)).toEqual(["build-tips"]);

		// Agent proposing a memory without rationale is rejected.
		await expect(
			owner.memories.proposeMemory({
				sessionId: s2.id,
				global: true,
				content: {
					slug: "missing-rationale",
					summary: "Should fail without rationale.",
					mode: MemoryMode.ALWAYS,
				},
			}),
		).rejects.toThrow(/--rationale/);

		// Supersede m1 immediately with a refined version.
		const m3 = await owner.memories.proposeMemory({
			sessionId: s2.id,
			global: true,
			supersedesIds: [m1.id],
			rationale: "User asked to clarify high tier",
			content: {
				slug: "use-neon",
				summary: "Use gemini-4-neon (high) for subagents.",
				mode: MemoryMode.ALWAYS,
			},
		});
		expect(m3.state).toBe(MemoryState.APPROVED);
		const oldM1 = await owner.memories.showMemory({ ref: String(m1.id) });
		expect(oldM1.state).toBe(MemoryState.SUPERSEDED);
		expect(oldM1.supersededBy).toBe(m3.id);

		// GetMemories resolves slug to m3 and fetches full body for build-tips,
		// omitting rationale/reason metadata from read context (while showMemory retains it).
		const fetched = await owner.memories.getMemories({
			project: "mem-proj",
			refs: ["use-neon", "build-tips"],
		});
		expect(fetched.memories.map((m) => m.id)).toEqual([m3.id, m2.id]);
		expect(fetched.memories[1].content?.body).toContain("pnpm build");
		expect(fetched.memories[0].provenance?.rationale).toBe("");
		expect(fetched.memories[0].proposed?.reason).toBe("");
		const shownM3 = await owner.memories.showMemory({ ref: String(m3.id) });
		expect(shownM3.provenance?.rationale).toBe("User asked to clarify high tier");

		// Permanently remove m2 — it disappears completely and is no longer returned on session start.
		const deleted = await owner.memories.deleteMemory({ id: m2.id });
		expect(deleted.id).toBe(m2.id);
		await expect(owner.memories.showMemory({ ref: String(m2.id) })).rejects.toThrow();

		const sAfterDelete = await owner.sessions.startSession({ project: "other-proj" });
		expect(sAfterDelete.memories?.onDemand).toEqual([]);
		expect(sAfterDelete.memories?.always.map((e) => e.slug)).toEqual(["use-neon"]);
	});
});

describe("BuildDistributor pinned controller version and placement semantics", () => {
	it("knows its own compiled/configured version, emits direct GitHub downloadUrl, and ignores stale SQLite cache", async () => {
		const { BuildDistributor } = await import("../src/builds.ts");
		const { buildCandidates, chooseCandidate } = await import("../src/placement.ts");
		const kvData = new Map<string, string>();
		const kv = {
			getMeta: (k: string) => kvData.get(k) ?? null,
			setMeta: (k: string, v: string) => {
				kvData.set(k, v);
			},
		};
		// Seed a stale SQLite cache from an older build 170.
		kv.setMeta(
			"github_latest_release",
			JSON.stringify({
				buildNumber: 170,
				tag: "build-170",
				fetchedAt: Date.now(),
				assets: {
					"hotbed-linux-amd64": { sha256: "oldsum", downloadUrl: "", apiUrl: "" },
				},
			}),
		);

		const amd64Sum = "a".repeat(64);
		const arm64Sum = "b".repeat(64);
		const darwinSum = "c".repeat(64);
		const guestdSum = "d".repeat(64);
		const dist = new BuildDistributor(
			{
				UPDATES_REPO: "semistrict/hotbed",
				CONTROLLER_BUILD_NUMBER: "172",
				CONTROLLER_GIT_COMMIT: "749b28361a430fa6f386ce3b838dfa0c92769954",
				CONTROLLER_BUILD_SHA256SUMS: [
					`${amd64Sum}  hotbed-linux-amd64`,
					`${arm64Sum}  hotbed-linux-arm64`,
					`${darwinSum}  hotbed-darwin-arm64`,
					`${guestdSum}  hotbed-guestd-linux-arm64`,
				].join("\n"),
			},
			kv,
		);

		expect(dist.pinnedBuildNumber()).toBe(172);
		expect(dist.currentBuildNumber()).toBe(172);
		expect(dist.controllerCommit()).toBe("749b28361a430fa6f386ce3b838dfa0c92769954");
		expect(dist.versionString()).toBe("build 172 (749b28361a43)");
		expect(dist.needsImmediateRefresh(Date.now())).toBe(false);
		expect(dist.needsBackgroundRefresh()).toBe(false);

		const b = dist.buildFor("linux", "amd64");
		expect(b).toBeDefined();
		expect(b!.number).toBe(172n);
		expect(b!.sha256).toBe(amd64Sum);
		expect(b!.path).toBe("/builds/172/hotbed-linux-amd64");
		expect(b!.downloadUrl).toBe(
			"https://github.com/semistrict/hotbed/releases/download/build-172/hotbed-linux-amd64",
		);

		// A Mac's worker gets the darwin build, and the Linux hotbed-guestd its
		// VMs boot.
		const mac = dist.buildFor("darwin", "arm64");
		expect(mac!.sha256).toBe(darwinSum);
		expect(mac!.path).toBe("/builds/172/hotbed-darwin-arm64");
		expect(mac!.guestd!.sha256).toBe(guestdSum);
		expect(mac!.guestd!.path).toBe("/builds/172/hotbed-guestd-linux-arm64");

		// Without baked sums, stale cache (build 170) is ignored for pinned build 172 and triggers immediate refresh.
		const unbaked = new BuildDistributor(
			{
				UPDATES_REPO: "semistrict/hotbed",
				CONTROLLER_BUILD_NUMBER: "172",
				CONTROLLER_GIT_COMMIT: "749b28361a430fa6f386ce3b838dfa0c92769954",
			},
			kv,
		);
		expect(unbaked.currentBuildNumber()).toBe(172);
		expect(unbaked.versionString()).toBe("build 172 (749b28361a43)");
		expect(unbaked.buildFor("linux", "amd64")).toBeUndefined();
		expect(unbaked.needsImmediateRefresh(Date.now())).toBe(true);
		expect(unbaked.needsBackgroundRefresh()).toBe(true);

		// Placement requires worker build >= controller build (accepts equal and newer, rejects older).
		const makeView = (id: string, hostname: string, version: string, runtimes = ["vm"]) => ({
			node: {
				id,
				hostname,
				asleep: false,
				online: true,
				storageError: "",
			} as never,
			addr: "100.0.0.1:7121",
			online: true,
			wakeFailed: false,
			bases: [],
			workspaces: [],
			commands: [],
			hb: create(HeartbeatRequestSchema, {
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 8,
					memoryBytes: BigInt(16 * 1024 ** 3),
					version,
					runtimes,
				}),
			}),
		});

		const cs = buildCandidates(
			[
				makeView("n-old", "old-worker", "build 171 (deadbeef1234)"),
				makeView("n-same", "same-worker", "build 172 (749b28361a43)"),
				makeView("n-newer", "newer-worker", "build 173 (cafebabe5678)"),
				makeView("n-unhealthy", "unhealthy-worker", "build 173 (cafebabe5678)", []),
			],
			"app",
			new Map(),
			() => 172,
		);
		expect(cs.find((c) => c.host === "old-worker")!.upgrading).toBe(171);
		expect(cs.find((c) => c.host === "same-worker")!.upgrading).toBe(0);
		expect(cs.find((c) => c.host === "newer-worker")!.upgrading).toBe(0);

		const pickNewer = chooseCandidate(cs, { node: "newer-worker" } as never, [], true);
		expect(pickNewer.err).toBeUndefined();
		expect(pickNewer.chosen?.host).toBe("newer-worker");

		const pickUnhealthy = chooseCandidate(cs, { node: "unhealthy-worker" } as never, [], true);
		expect(pickUnhealthy.err).toBeDefined();
		expect(pickUnhealthy.err?.message).toContain("has no available runtimes");

		const pickOld = chooseCandidate(cs, { node: "old-worker" } as never, [], true);
		expect(pickOld.err).toBeDefined();
		expect(pickOld.err?.message).toContain("build 171");

		// Placement steers new work away from a node with parked commands even
		// when it is not currently holding and has low CPU busy.
		const busyParked = makeView("n-parked", "parked-worker", "build 172 (749b28361a43)");
		busyParked.commands = [
			{ id: "c1", state: CommandState.PARKED } as never,
			{ id: "c2", state: CommandState.PARKED } as never,
		];
		const cleanNode = makeView("n-clean", "clean-worker", "build 172 (749b28361a43)");
		const csParked = buildCandidates(
			[busyParked, cleanNode],
			"app",
			new Map([["n-parked", "ws-existing"]]),
			() => 172,
		);
		const pickedClean = chooseCandidate(csParked, undefined, [], true);
		expect(pickedClean.err).toBeUndefined();
		expect(pickedClean.chosen?.host).toBe("clean-worker");

		// Private repos (GITHUB_TOKEN set) omit direct downloadUrl so clients use /builds/ proxy,
		// and force-refresh on a baked pinned build returns the baked release without calling GitHub.
		const priv = new BuildDistributor(
			{
				UPDATES_REPO: "semistrict/hotbed",
				GITHUB_TOKEN: "ghp_private_repo_token",
				UPDATES_API_BASE: "http://127.0.0.1:1",
				CONTROLLER_BUILD_NUMBER: "172",
				CONTROLLER_BUILD_SHA256SUMS: `${amd64Sum}  hotbed-linux-amd64\n`,
			},
			kv,
		);
		const privBuild = priv.buildFor("linux", "amd64");
		expect(privBuild?.downloadUrl).toBe("");
		expect(privBuild?.path).toBe("/builds/172/hotbed-linux-amd64");
		const refreshed = await priv.refreshIfStale(Date.now(), true);
		expect(refreshed?.buildNumber).toBe(172);
	});
});

describe("Security, cron, daemon logs, tunnel, and workspace regression tests", () => {
	it("pins worker-token hostname, blocks JOIN_TOKEN on /ws/worker, and scopes worker Standby", async () => {
		const owner = ownerClients();
		const mintA = await SELF.fetch("https://controller.test/admin/worker-token", {
			method: "POST",
			headers: {
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"Content-Type": "application/json",
			},
			body: JSON.stringify({ nodeId: "node-iso-a", hostname: "host-a" }),
		});
		expect(mintA.status).toBe(201);
		const { token: tokenA } = (await mintA.json()) as { token: string };

		const joinA = workerClients(JOIN_TOKEN, "node-iso-a", "host-a", "100.73.156.51:7121");
		const joinB = workerClients(JOIN_TOKEN, "node-iso-b", "host-b", "100.73.156.52:7121");
		const jA = await joinA.cluster.join({});
		await joinB.cluster.join({});

		// JOIN_TOKEN must not open /ws/worker and hijack an existing node's tunnel.
		const hijackRes = await SELF.fetch("https://controller.test/ws/worker", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${JOIN_TOKEN}`,
				"X-Hotbed-Node-Id": "node-iso-b",
				"X-Hotbed-Host": "host-b",
			},
		});
		expect(hijackRes.status).toBe(403);

		// Project restricted strictly to host-b.
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "secret-b",
				url: "https://github.com/example/secret-b.git",
				branch: "main",
			}),
		});
		await owner.cluster.setProjectHosts({ name: "secret-b", hosts: ["host-b"] });

		// Worker A spoofing X-Hotbed-Host: host-b still resolves to DB-bound host-a and does not receive secret-b.
		const spoofedA = workerClients(tokenA, "node-iso-a", "host-b", "100.73.156.51:7121");
		const hbA = await spoofedA.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: jA.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 4,
					memoryBytes: BigInt(8 * 1024 ** 3),
				}),
			}),
		);
		expect(hbA.projects.map((p) => p.name)).not.toContain("secret-b");

		// Worker A cannot supervise host-b via Standby.
		const drainStandby = async () => {
			for await (const _ of spoofedA.cluster.standby({
				node: "host-b",
				running: false,
				sleepAfterSeconds: 60n,
			})) {
				// should throw before yielding
			}
		};
		await expect(drainStandby()).rejects.toThrow(/may not supervise host-b/);
	});

	it("enforces CSRF on POST /auth/logout while allowing top-level GET navigations to Cockpit UI", async () => {
		const loginRes = await SELF.fetch("https://controller.test/auth/login", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ token: OWNER_TOKEN }),
			redirect: "manual",
		});
		const cookieVal = (loginRes.headers.get("Set-Cookie") ?? "").split(";")[0];

		// Cross-site POST /auth/logout is rejected.
		const badLogout = await SELF.fetch("https://controller.test/auth/logout", {
			method: "POST",
			headers: { Cookie: cookieVal, "Sec-Fetch-Site": "cross-site" },
			redirect: "manual",
		});
		expect(badLogout.status).toBe(403);

		// Top-level document navigation to Cockpit UI succeeds (e.g. post Cloudflare Access redirect).
		const navRes = await SELF.fetch("https://controller.test/", {
			headers: {
				Cookie: cookieVal,
				"Sec-Fetch-Site": "cross-site",
				"Sec-Fetch-Mode": "navigate",
				"Sec-Fetch-Dest": "document",
			},
		});
		expect(navRes.status).toBe(200);

		// Non-navigation cross-site fetch to Cockpit UI is still rejected.
		const corsRes = await SELF.fetch("https://controller.test/", {
			headers: {
				Cookie: cookieVal,
				"Sec-Fetch-Site": "cross-site",
				"Sec-Fetch-Mode": "cors",
			},
		});
		expect(corsRes.status).toBe(401);
	});

	it("handles DST spring-forward and DOW 7-7 cleanly in parseCron", async () => {
		const { parseCron } = await import("../src/cron.ts");

		// 2026-03-08 is DST spring-forward in America/New_York (23-hour day).
		// 00:30 on March 9 must NOT be skipped to 2027.
		const march8MidnightEst = Date.parse("2026-03-08T05:00:00Z");
		const dstCron = parseCron("CRON_TZ=America/New_York 30 0 9 3 *", march8MidnightEst);
		const nextMs = dstCron.next(march8MidnightEst);
		expect(nextMs).toBe(Date.parse("2026-03-09T04:30:00Z")); // 2026-03-09 00:30 EDT

		// DOW 7-7 must match Sunday only, not Monday through Saturday.
		const sunCron = parseCron("0 9 * * 7-7", Date.parse("2026-11-23T00:00:00Z"));
		expect([...sunCron.dow]).toEqual([0]);
		const nextSun = sunCron.next(Date.parse("2026-11-23T00:00:00Z")); // Monday Nov 23, 2026
		expect(nextSun).toBe(Date.parse("2026-11-29T09:00:00Z")); // Sunday Nov 29, 2026
	});

	it("filters daemon_logs by query in SQL before applying LIMIT", async () => {
		const { DaemonLogLevel } = await import("../src/gen/hotbed/v1/hotbed_pb.ts");
		const owner = ownerClients();
		const stub = env.CONTROLLER.getByName("default");
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec("DELETE FROM daemon_logs");
			state.storage.sql.exec(
				"INSERT INTO daemon_logs (at, level, msg, attrs_json) VALUES (?, ?, ?, ?)",
				1000,
				DaemonLogLevel.WARN,
				"needle match older",
				"",
			);
			for (let i = 1; i <= 5; i++) {
				state.storage.sql.exec(
					"INSERT INTO daemon_logs (at, level, msg, attrs_json) VALUES (?, ?, ?, ?)",
					1000 + i * 1000,
					DaemonLogLevel.WARN,
					`noise entry ${i}`,
					"",
				);
			}
		});
		const res = await owner.workspaces.getDaemonLogs({
			source: "controller",
			query: "needle",
			limit: 2,
		});
		expect(res.entries.map((e) => e.msg)).toEqual(["needle match older"]);
		expect(res.truncated).toBe(false);
	});

	it("keeps workspace row released for toRelease() when session ends during CreateWorkspace", async () => {
		const owner = ownerClients();
		const worker = workerClients(OWNER_TOKEN, "node-race-ws", "race-ws", "100.73.156.62:7121");
		const joined = await worker.cluster.join({});
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "race-demo",
				url: "https://github.com/example/race-demo.git",
				branch: "main",
			}),
		});
		await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 4,
					memoryBytes: BigInt(8 * 1024 ** 3),
					runtimes: ["vm"],
				}),
			}),
		);
		const sess = await owner.sessions.startSession({ project: "race-demo" });

		// Intercept CreateWorkspace, end the session while it is in-flight, and fail ReleaseWorkspace.
		const res = await SELF.fetch("https://controller.test/ws/worker", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"X-Hotbed-Node-Id": "node-race-ws",
				"X-Hotbed-Host": "race-ws",
				"X-Hotbed-Addr": "100.73.156.62:7121",
			},
		});
		expect(res.status).toBe(101);
		const ws = res.webSocket!;
		ws.accept();
		let createdWsId = "";
		ws.addEventListener("message", (ev) => {
			const msg = JSON.parse(String(ev.data)) as TunnelDownMessage;
			if (msg.type !== "rpc_req") return;
			if (msg.procedure === "/hotbed.v1.WorkspaceService/CreateWorkspace") {
				const req = fromBinary(CreateWorkspaceRequestSchema, decodeBase64(msg.body));
				createdWsId = req.id;
				void owner.sessions.endSession({ id: sess.id }).then(() => {
					const reply = encodeProto(
						WorkspaceSchema,
						create(WorkspaceSchema, {
							id: req.id,
							project: "race-demo",
							sessionId: sess.id,
							node: "race-ws",
							state: WorkspaceState.ACTIVE,
							createdAt: BigInt(Date.now()),
						}),
					);
					ws.send(
						JSON.stringify({
							type: "rpc_msg",
							id: msg.id,
							body: encodeBase64(reply),
						} satisfies TunnelUpMessage),
					);
				});
				return;
			}
			if (msg.procedure === "/hotbed.v1.WorkspaceService/ReleaseWorkspace") {
				ws.send(
					JSON.stringify({
						type: "rpc_err",
						id: msg.id,
						code: "WORKER_UNREACHABLE",
						message: "simulated transient release failure",
						retryable: true,
					} satisfies TunnelUpMessage),
				);
			}
		});

		try {
			await expect(
				owner.workspaces.createWorkspace({
					sessionId: sess.id,
					placement: { node: "race-ws" },
				}),
			).rejects.toThrow(/ended/);

			expect(createdWsId).not.toBe("");

			// Worker reports the workspace in its checkpoint; heartbeat must instruct it to release createdWsId.
			await worker.cluster.pushEvents({
				clusterId: joined.clusterId,
				epoch: "race-e1",
				checkpoint: {
					seq: 1n,
					bases: [],
					workspaces: [
						create(WorkspaceSchema, {
							id: createdWsId,
							project: "race-demo",
							sessionId: sess.id,
							node: "race-ws",
							state: WorkspaceState.ACTIVE,
							createdAt: BigInt(Date.now()),
						}),
					],
					commands: [],
				},
			});
			const hb = await worker.cluster.heartbeat(
				create(HeartbeatRequestSchema, {
					clusterId: joined.clusterId,
					resources: create(ResourcesSchema, {
						arch: "amd64",
						cpus: 4,
						memoryBytes: BigInt(8 * 1024 ** 3),
					}),
				}),
			);
			expect(hb.release).toContain(createdWsId);
		} finally {
			ws.close(1000, "done");
		}
	});

	it("rejects malformed base64 tunnel frames cleanly instead of stranding unary RPCs", async () => {
		const owner = ownerClients();
		const worker = workerClients(OWNER_TOKEN, "node-badb64", "badb64", "100.73.156.61:7121");
		const joined = await worker.cluster.join({});
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "badb64-demo",
				url: "https://github.com/example/badb64-demo.git",
				branch: "main",
			}),
		});
		await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 4,
					memoryBytes: BigInt(8 * 1024 ** 3),
					runtimes: ["vm"],
				}),
			}),
		);
		const sess = await owner.sessions.startSession({ project: "badb64-demo" });

		// Connect a mock tunnel that replies with invalid base64 to CreateWorkspace.
		const res = await SELF.fetch("https://controller.test/ws/worker", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${OWNER_TOKEN}`,
				"X-Hotbed-Node-Id": "node-badb64",
				"X-Hotbed-Host": "badb64",
				"X-Hotbed-Addr": "100.73.156.61:7121",
			},
		});
		expect(res.status).toBe(101);
		const ws = res.webSocket!;
		ws.accept();
		ws.addEventListener("message", (ev) => {
			const msg = JSON.parse(String(ev.data)) as TunnelDownMessage;
			if (msg.type === "rpc_req") {
				ws.send(
					JSON.stringify({
						type: "rpc_msg",
						id: msg.id,
						body: "!!!not-valid-base64!!!",
					} satisfies TunnelUpMessage),
				);
			}
		});
		try {
			await expect(
				owner.workspaces.createWorkspace({
					sessionId: sess.id,
					placement: { node: "badb64" },
				}),
			).rejects.toThrow(/invalid base64 payload/);
		} finally {
			ws.close(1000, "done");
		}
	});

	it("computes R2 upload/download rates, 24h rollups, and restart deltas without first-sample spikes", async () => {
		const { buildSeries } = await import("../src/services/cluster.ts");
		const now = 1_760_000_000_000;
		const base = {
			node: "n1",
			cpu: 0.2,
			memory: 0.5,
			psi: 0,
			cpuPsi: 0,
			holding: false,
			running: 0,
			queued: 0,
			parked: 0,
			oom: 0,
		};
		const samples = [
			// Pre-upgrade sample (no R2 counters).
			{
				...base,
				t: now - 30_000,
				r2Up: 0,
				r2Down: 0,
				r2Puts: 0,
				r2Gets: 0,
				r2Fails: 0,
				r2WaitNs: 0,
				peerGiven: 0,
				peerServed: 0,
				peerGivenBytes: 0,
				peerServedBytes: 0,
			},
			// First R2 sample carries existing since-boot totals: must establish baseline with 0 delta.
			{
				...base,
				t: now - 20_000,
				r2Up: 20_000_000,
				r2Down: 5_000_000,
				r2Puts: 100,
				r2Gets: 50,
				r2Fails: 0,
				r2WaitNs: 1_000_000_000,
				peerGiven: 1000,
				peerServed: 200,
				peerGivenBytes: 8_000_000,
				peerServedBytes: 1_600_000,
			},
			// 10 seconds later: +10 MiB up, +2 MiB down, +10 puts, +4 gets, +1 fail, +500 ms commit wait,
			// +100 parts (1 MB) from peers, +30 (300 KB) served to peers.
			{
				...base,
				t: now - 10_000,
				r2Up: 30_000_000,
				r2Down: 7_000_000,
				r2Puts: 110,
				r2Gets: 54,
				r2Fails: 1,
				r2WaitNs: 1_500_000_000,
				peerGiven: 1100,
				peerServed: 230,
				peerGivenBytes: 9_000_000,
				peerServedBytes: 1_900_000,
			},
			// Daemon restarted and uploaded 4 MiB more in 10s.
			{
				...base,
				t: now,
				r2Up: 4_000_000,
				r2Down: 1_000_000,
				r2Puts: 5,
				r2Gets: 2,
				r2Fails: 0,
				r2WaitNs: 0,
				peerGiven: 40,
				peerServed: 10,
				peerGivenBytes: 400_000,
				peerServedBytes: 100_000,
			},
		];
		const { series, nodeHourly, r2Totals } = buildSeries(samples, now);
		expect(r2Totals.up).toBe(14_000_000);
		expect(r2Totals.down).toBe(3_000_000);
		expect(r2Totals.puts).toBe(15);
		expect(r2Totals.gets).toBe(6);
		expect(r2Totals.fails).toBe(1);
		expect(r2Totals.waitNs).toBe(500_000_000);
		expect(r2Totals.peerGiven).toBe(140);
		expect(r2Totals.peerServed).toBe(40);
		expect(r2Totals.peerGivenBytes).toBe(1_400_000);
		expect(r2Totals.peerServedBytes).toBe(400_000);

		const pts = series.n1.samples;
		expect(pts).toHaveLength(4);
		expect(pts[1].uploadBytesPerSec).toBe(0);
		expect(pts[2].uploadBytesPerSec).toBeCloseTo(1_000_000, 1);
		expect(pts[2].downloadBytesPerSec).toBeCloseTo(200_000, 1);
		expect(pts[2].commitWaitMsPerSec).toBeCloseTo(50, 1);
		expect(pts[2].peerPartsGivenPerSec).toBeCloseTo(10, 5);
		expect(pts[2].peerPartsServedPerSec).toBeCloseTo(3, 5);
		expect(pts[2].r2GetsPerSec).toBeCloseTo(0.4, 5);
		expect(pts[2].peerGivenBytesPerSec).toBeCloseTo(100_000, 1);
		expect(pts[2].peerServedBytesPerSec).toBeCloseTo(30_000, 1);
		expect(pts[1].peerPartsGivenPerSec).toBe(0);
		expect(pts[3].uploadBytesPerSec).toBeCloseTo(400_000, 1);

		const hourSumUp = nodeHourly.n1.hours.reduce((acc, h) => acc + Number(h.uploadBytes), 0);
		expect(hourSumUp).toBe(14_000_000);
	});

	it("manages centralized worker & VM config, merges into HeartbeatResponse, and attaches Build.guestd", async () => {
		const owner = ownerClients();
		const worker = workerClients(OWNER_TOKEN, "node-cfg-1", "ubu1", "100.73.156.10:7121");
		const joined = await worker.cluster.join({});
		const workerWithToken = workerClients(
			joined.workerToken || OWNER_TOKEN,
			"node-cfg-1",
			"ubu1",
			"100.73.156.10:7121",
		);
		const kernelSha = "a".repeat(64);
		const guestdSha = "b".repeat(64);
		const hotbedSha = "c".repeat(64);

		// Set cluster-wide defaults with a managed amd64 guest kernel.
		const defCfg = await owner.cluster.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				defaultRuntime: "vm",
				vm: create(WorkerVMConfigSchema, {
					vcpus: 8,
					memMib: 12288,
					cacheType: "Unsafe",
					nofile: 65536n,
					kernels: {
						amd64: create(ManagedArtifactSchema, {
							url: "https://example.com/vmlinux-kvm",
							sha256: kernelSha,
							version: "cladefs-6.12-kvm",
						}),
					},
				}),
			}),
		});
		expect(defCfg.revision).toBe(1n);

		// Per-node override for ubu1 overrides memMib while inheriting vcpus & kernel.
		const nodeCfg = await owner.cluster.setWorkerConfig({
			node: "ubu1",
			config: create(WorkerConfigSchema, {
				vm: create(WorkerVMConfigSchema, {
					enabled: true,
					memMib: 16384,
					cladefsPeerAddr: "100.73.156.10:9185",
				}),
			}),
		});
		expect(nodeCfg.node).toBe("ubu1");
		expect(nodeCfg.revision).toBe(1n);

		// Workers cannot mutate cluster config.
		await expect(
			workerWithToken.cluster.setWorkerConfig({
				node: "ubu1",
				config: create(WorkerConfigSchema, { defaultRuntime: "cladefs" }),
			}),
		).rejects.toThrow(/not authorized|may not modify/);

		// Removed chroot default_runtime is rejected.
		await expect(
			owner.cluster.setWorkerConfig({
				node: "ubu1",
				config: create(WorkerConfigSchema, { defaultRuntime: "chroot" }),
			}),
		).rejects.toThrow(/invalid default_runtime "chroot"/);

		// Configure baked SHA256SUMS including hotbed-guestd-linux-amd64.
		const id = env.CONTROLLER.idFromName("default");
		const stub = env.CONTROLLER.get(id);
		await runInDurableObject(stub, async (instance) => {
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			(instance as any).builds.setTestConfig({
				updatesRepo: "semistrict/hotbed",
				updatesToken: "",
				controllerBuild: 178,
				controllerSha256Sums: [
					`${hotbedSha}  hotbed-linux-amd64`,
					`${guestdSha}  hotbed-guestd-linux-amd64`,
				].join("\n"),
			});
		});

		// Heartbeat delivers merged effective config AND Build.guestd for linux/amd64.
		const hb = await workerWithToken.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(27 * 1024 ** 3),
				}),
			}),
		);
		expect(hb.workerConfig?.defaultRuntime).toBe("vm");
		expect(hb.workerConfig?.vm?.enabled).toBe(true);
		expect(hb.workerConfig?.vm?.vcpus).toBe(8);
		expect(hb.workerConfig?.vm?.memMib).toBe(16384);
		expect(hb.workerConfig?.vm?.nofile).toBe(65536n);
		expect(hb.workerConfig?.vm?.kernels?.amd64?.sha256).toBe(kernelSha);
		expect(hb.build?.number).toBe(178n);
		expect(hb.build?.guestd?.sha256).toBe(guestdSha);
		expect(hb.build?.guestd?.url).toBe(
			"https://github.com/semistrict/hotbed/releases/download/build-178/hotbed-guestd-linux-amd64",
		);
		expect(hb.build?.guestd?.path).toBe("/builds/178/hotbed-guestd-linux-amd64");

		// GetWorkerConfig returns defaults, node override, and merged effective view.
		const got = await owner.cluster.getWorkerConfig({ node: "ubu1" });
		expect(got.effective?.vm?.vcpus).toBe(8);
		expect(got.effective?.vm?.memMib).toBe(16384);

		// Delete per-node override and defaults.
		const delNode = await owner.cluster.deleteWorkerConfig({ node: "ubu1" });
		expect(delNode.deleted).toBe(true);
		const delDef = await owner.cluster.deleteWorkerConfig({ defaults: true });
		expect(delDef.deleted).toBe(true);
	});

	it("validates and merges WorkerFreshenConfig and isolates shared_bases by runtime", async () => {
		const owner = ownerClients();

		// 1. WorkerFreshenConfig validation rejects invalid duty cycle and negative seconds.
		await expect(
			owner.cluster.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					freshen: create(WorkerFreshenConfigSchema, { maxDutyCycle: 0 }),
				}),
			}),
		).rejects.toThrow(/max_duty_cycle/);
		await expect(
			owner.cluster.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					freshen: create(WorkerFreshenConfigSchema, { maxDutyCycle: 1.5 }),
				}),
			}),
		).rejects.toThrow(/max_duty_cycle/);
		await expect(
			owner.cluster.setWorkerConfig({
				config: create(WorkerConfigSchema, {
					freshen: create(WorkerFreshenConfigSchema, { minIntervalSeconds: -1 }),
				}),
			}),
		).rejects.toThrow(/min_interval_seconds/);

		// Valid defaults + per-node override merge cleanly.
		await owner.cluster.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				freshen: create(WorkerFreshenConfigSchema, {
					maxDutyCycle: 0.05,
					minIntervalSeconds: 1800,
					maxCooldownSeconds: 21600,
					idleDelaySeconds: 15,
				}),
			}),
		});
		await owner.cluster.setWorkerConfig({
			node: "arm-vm",
			config: create(WorkerConfigSchema, {
				freshen: create(WorkerFreshenConfigSchema, {
					maxDutyCycle: 0.08,
					disableOptional: false,
				}),
			}),
		});
		const cfgView = await owner.cluster.getWorkerConfig({ node: "arm-vm" });
		expect(cfgView.effective?.freshen?.maxDutyCycle).toBe(0.08);
		expect(cfgView.effective?.freshen?.minIntervalSeconds).toBe(1800);
		expect(cfgView.effective?.freshen?.idleDelaySeconds).toBe(15);

		// 2. shared_bases are scoped by runtime so arm64 vm and arm64 cladefs never shadow each other.
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "iso-proj",
				url: "https://github.com/semistrict/iso-proj.git",
			}),
		});
		await owner.cluster.addProject({
			name: "iso-proj",
			repos: ["github.com/semistrict/iso-proj"],
		});
		const vmNode = workerClients(OWNER_TOKEN, "node-arm-vm", "arm-vm", "100.73.156.51:7121");
		const fuseNode = workerClients(OWNER_TOKEN, "node-arm-fuse", "arm-fuse", "100.73.156.52:7121");
		const vm2Node = workerClients(OWNER_TOKEN, "node-arm-vm2", "arm-vm2", "100.73.156.53:7121");
		const j = await vmNode.cluster.join({});
		await fuseNode.cluster.join({});
		await vm2Node.cluster.join({});

		// arm-vm advertises a VM shared base at t=1.
		await vmNode.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j.clusterId,
				resources: create(ResourcesSchema, {
					arch: "arm64",
					cpus: 8,
					memoryBytes: BigInt(16 * 1024 ** 3),
					runtimes: ["vm"],
				}),
				heldBases: [
					create(SharedBaseSchema, {
						cell: create(CladefsCellSchema, {
							project: "iso-proj",
							arch: "arm64",
							baseId: "iso-proj-vm-11111111",
							recipe: "vm-recipe-1",
							runtime: "vm",
							tenant: 0n,
							cell: 101n,
							version: 11n,
						}),
					}),
				],
			}),
		);
		// arm-fuse advertises a NEWER cladefs shared base on the same arch (arm64).
		await fuseNode.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j.clusterId,
				resources: create(ResourcesSchema, {
					arch: "arm64",
					cpus: 8,
					memoryBytes: BigInt(16 * 1024 ** 3),
					runtimes: ["cladefs"],
				}),
				heldBases: [
					create(SharedBaseSchema, {
						cell: create(CladefsCellSchema, {
							project: "iso-proj",
							arch: "arm64",
							baseId: "iso-proj-fuse-22222222",
							recipe: "fuse-recipe-1",
							runtime: "cladefs",
							tenant: 0n,
							cell: 202n,
							version: 22n,
						}),
					}),
				],
			}),
		);
		// Second arm64 vm node joins with no local base.
		await vm2Node.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: j.clusterId,
				resources: create(ResourcesSchema, {
					arch: "arm64",
					cpus: 8,
					memoryBytes: BigInt(16 * 1024 ** 3),
					runtimes: ["vm"],
				}),
			}),
		);

		const capturedVM: Array<MessageShape<typeof CreateWorkspaceRequestSchema>> = [];
		const vmTunnel = await openMockWorkerTunnel(
			OWNER_TOKEN,
			"node-arm-vm2",
			"arm-vm2",
			"100.73.156.53:7121",
			{
				unary: {
					"/hotbed.v1.WorkspaceService/CreateWorkspace": (reqBytes) => {
						const req = fromBinary(CreateWorkspaceRequestSchema, reqBytes);
						capturedVM.push(req);
						return encodeProto(
							WorkspaceSchema,
							create(WorkspaceSchema, {
								id: req.id,
								sessionId: req.sessionId,
								nodeId: "node-arm-vm2",
								project: "iso-proj",
								runtime: "vm",
								state: WorkspaceState.IDLE,
							}),
						);
					},
				},
			},
		);
		try {
			const sess = await owner.sessions.startSession({ project: "iso-proj" });
			await owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "iso-proj",
				placement: { node: "arm-vm2", runtime: "vm" },
			});
			expect(capturedVM).toHaveLength(1);
			// Must receive the arm64 VM shared base (cell 101), NOT the newer cladefs cell (202)!
			expect(capturedVM[0].sharedBase?.cell?.cell).toBe(101n);
			expect(capturedVM[0].sharedBase?.cell?.runtime).toBe("vm");
		} finally {
			vmTunnel.close();
		}
	});

	it("keeps one chain of shared bases per project, arch and runtime, and retires replaced bases no worker holds", async () => {
		const owner = ownerClients();
		const a = workerClients(OWNER_TOKEN, "node-chain-a", "chain-a", "100.73.157.1:7121");
		const b = workerClients(OWNER_TOKEN, "node-chain-b", "chain-b", "100.73.157.2:7121");
		const j = await a.cluster.join({});
		await b.cluster.join({});
		const stub = env.CONTROLLER.getByName("default");
		let clock = Date.now();
		const tenantBefore = await runInDurableObject(stub, async (instance) => {
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			const ctrl = (instance as any).ctrl;
			ctrl.nowFn = () => clock;
			return ctrl.cladefsTenantId as bigint | null;
		});
		const base = (cell: bigint, tenant = 0n) =>
			create(SharedBaseSchema, {
				cell: create(CladefsCellSchema, {
					project: "chain-proj",
					arch: "arm64",
					baseId: `chain-proj-vm-${cell}`,
					recipe: `r${cell}`,
					runtime: "vm",
					tenant,
					cell,
					version: 1n,
				}),
				repos: { "chain-proj": `c${cell}` },
			});
		const beat = async (
			w: ReturnType<typeof workerClients>,
			held: ReturnType<typeof base>[],
			retired: bigint[] = [],
		) => {
			const hb = await w.cluster.heartbeat(
				create(HeartbeatRequestSchema, {
					clusterId: j.clusterId,
					resources: create(ResourcesSchema, {
						arch: "arm64",
						cpus: 8,
						memoryBytes: BigInt(16 * 1024 ** 3),
						runtimes: ["vm"],
					}),
					heldBases: held,
					retiredBases: retired.map((c) => base(c).cell!),
				}),
			);
			return hb.retireBases.map((c) => c.cell);
		};
		const latest = () =>
			runInDurableObject(
				stub,
				async (instance) =>
					// eslint-disable-next-line @typescript-eslint/no-explicit-any
					(instance as any).store.latestSharedBase("chain-proj", "arm64", 0n, "vm")?.cell?.cell,
			);
		try {
			// a builds base 1; b takes it up; a builds base 2 on it.
			await beat(a, [base(1n)]);
			await beat(b, [base(1n)]);
			expect(await beat(a, [base(2n)])).toEqual([]);
			expect(await latest()).toBe(2n);

			// The newest is the one heard of last, whoever heartbeats last.
			await beat(b, [base(1n)]);
			expect(await latest()).toBe(2n);

			// b still holds base 1, so it stays, however long.
			clock += 60 * 60_000;
			await beat(b, [base(1n)]);
			expect(await beat(a, [base(2n)])).toEqual([]);

			// b moves to base 2: base 1, held by nobody, stays through the grace...
			await beat(b, [base(2n)]);
			clock += 14 * 60_000;
			expect(await beat(a, [base(2n)])).toEqual([]);

			// ...then its builder, a, is asked to retire it; b is not.
			clock += 2 * 60_000;
			expect(await beat(b, [base(2n)])).toEqual([]);
			expect(await beat(a, [base(2n)])).toEqual([1n]);

			// a retired it, and is asked no more.
			await beat(a, [base(2n)], [1n]);
			expect(await beat(a, [base(2n)])).toEqual([]);

			// The newest base stays, held or not.
			await beat(a, []);
			await beat(b, []);
			clock += 60 * 60_000;
			await beat(b, []);
			expect(await beat(a, [])).toEqual([]);
			expect(await latest()).toBe(2n);

			// A base replaced just now stays through the grace, however long
			// nobody held it: some node may be forking it as the newest.
			await beat(a, [base(3n)]);
			expect(await beat(a, [base(3n)])).toEqual([]);
			clock += 16 * 60_000;
			expect(await beat(a, [base(3n)])).toEqual([2n]);
			await beat(a, [base(3n)], [2n]);

			// A heartbeat that cannot say which bases its worker holds (its cladefs
			// runtime is down) keeps the holds it last reported.
			await beat(a, [base(4n)]);
			await beat(b, [base(3n)]);
			await b.cluster.heartbeat(
				create(HeartbeatRequestSchema, {
					clusterId: j.clusterId,
					resources: create(ResourcesSchema, { arch: "arm64", cpus: 8, runtimes: [] }),
				}),
			);
			clock += 60 * 60_000;
			expect(await beat(a, [base(4n)])).toEqual([]);

			// The bases a retired node built pass to the next node that heartbeats:
			// b deletes base 3 once a is gone and b no longer holds it.
			clock += 10 * 60_000;
			await beat(b, [base(4n)]);
			await owner.cluster.retireNode({ node: "chain-a" });
			clock += 16 * 60_000;
			expect(await beat(b, [base(4n)])).toEqual([3n]);

			// A base of another tenant is no base of this controller's.
			await runInDurableObject(stub, async (instance) => {
				// eslint-disable-next-line @typescript-eslint/no-explicit-any
				(instance as any).ctrl.cladefsTenantId = 0n;
			});
			await beat(b, [base(4n), base(5n, 7n)]);
			expect(await latest()).toBe(4n);
		} finally {
			await runInDurableObject(stub, async (instance) => {
				// eslint-disable-next-line @typescript-eslint/no-explicit-any
				const ctrl = (instance as any).ctrl;
				ctrl.nowFn = () => Date.now();
				ctrl.cladefsTenantId = tenantBefore;
			});
		}
	});

	it("validates WorkerSystemConfig and orchestrates explicit system upgrade over worker tunnel", async () => {
		const owner = ownerClients();
		const worker = workerClients(OWNER_TOKEN, "node-sys-1", "ubu-sys-1", "100.73.156.99:7121");
		const joined = await worker.cluster.join({});
		const workerWithToken = workerClients(
			joined.workerToken || OWNER_TOKEN,
			"node-sys-1",
			"ubu-sys-1",
			"100.73.156.99:7121",
		);
		await workerWithToken.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(27 * 1024 ** 3),
				}),
			}),
		);

		const cladefsdSha = "d".repeat(64);
		const fcSha = "e".repeat(64);

		// Workers cannot trigger system upgrades or call WorkspaceService.upgradeSystem directly.
		await expect(workerWithToken.cluster.upgradeSystem({ node: "ubu-sys-1" })).rejects.toThrow(
			/not authorized|may not trigger/,
		);
		await expect(owner.workspaces.upgradeSystem({})).rejects.toThrow(
			/permission_denied|internal controller-to-worker/,
		);

		// Without artifacts configured for amd64, node is skipped cleanly.
		const skipped = await owner.cluster.upgradeSystem({ node: "ubu-sys-1" });
		expect(skipped.nodes).toHaveLength(1);
		expect(skipped.nodes[0].node).toBe("ubu-sys-1");
		expect(skipped.nodes[0].status).toBe("skipped");

		// Configure cluster-wide system.cladefsd + system.firecracker for amd64.
		await owner.cluster.setWorkerConfig({
			config: create(WorkerConfigSchema, {
				system: create(WorkerSystemConfigSchema, {
					cladefsd: {
						amd64: create(ManagedArtifactSchema, {
							url: "https://example.com/cladefsd-linux-amd64",
							sha256: cladefsdSha,
							version: "cladefs-d3043d4a",
						}),
					},
					firecracker: {
						amd64: create(ManagedArtifactSchema, {
							url: "https://example.com/firecracker-linux-amd64",
							sha256: fcSha,
							version: "fc-d3043d4a",
						}),
					},
				}),
			}),
		});

		// Open worker tunnel and handle /hotbed.v1.WorkspaceService/UpgradeSystem.
		const receivedReqs: Array<MessageShape<typeof WorkerUpgradeSystemRequestSchema>> = [];
		const tunnel = await openMockWorkerTunnel(
			joined.workerToken || OWNER_TOKEN,
			"node-sys-1",
			"ubu-sys-1",
			"100.73.156.99:7121",
			{
				unary: {
					"/hotbed.v1.WorkspaceService/UpgradeSystem": (reqBytes) => {
						const wreq = fromBinary(WorkerUpgradeSystemRequestSchema, reqBytes);
						receivedReqs.push(wreq);
						return toBinary(
							NodeSystemUpgradeSchema,
							create(NodeSystemUpgradeSchema, {
								node: "ubu-sys-1",
								arch: "amd64",
								status: "upgraded",
								cladefsdVersion: wreq.cladefsd?.version ?? "",
								cladefsdSha256: wreq.cladefsd?.sha256 ?? "",
								cladefsdUpdated: true,
								firecrackerVersion: wreq.firecracker?.version ?? "",
								firecrackerSha256: wreq.firecracker?.sha256 ?? "",
								firecrackerUpdated: true,
								restartedCladefsd: true,
								cladefsIncarnation: 9n,
							}),
						);
					},
				},
			},
		);

		try {
			const upgraded = await owner.cluster.upgradeSystem({ node: "ubu-sys-1" });
			expect(upgraded.nodes).toHaveLength(1);
			expect(upgraded.nodes[0].status).toBe("upgraded");
			expect(upgraded.nodes[0].cladefsdUpdated).toBe(true);
			expect(upgraded.nodes[0].firecrackerUpdated).toBe(true);
			expect(upgraded.nodes[0].restartedCladefsd).toBe(true);
			expect(upgraded.nodes[0].cladefsIncarnation).toBe(9n);
			expect(receivedReqs).toHaveLength(1);
			expect(receivedReqs[0].cladefsd?.sha256).toBe(cladefsdSha);
			expect(receivedReqs[0].firecracker?.sha256).toBe(fcSha);

			// Cluster events log records the upgrade event.
			const evs = await owner.cluster.listEvents({ node: "ubu-sys-1", kinds: ["upgrade"] });
			expect(evs.events.some((e) => e.text.includes("system upgrade"))).toBe(true);
		} finally {
			tunnel.close();
		}
	});

	it("streams T3-linked question outbox snapshots and live updates over /ws/questions", async () => {
		const owner = ownerClients();

		// 1. Auth & upgrade enforcement on /ws/questions
		const noUpgrade = await SELF.fetch("https://controller.test/ws/questions", {
			headers: { Authorization: `Bearer ${OWNER_TOKEN}` },
		});
		expect(noUpgrade.status).toBe(426);

		const unauth = await SELF.fetch("https://controller.test/ws/questions", {
			headers: { Upgrade: "websocket" },
		});
		expect(unauth.status).toBe(403);

		// Worker token is rejected on /ws/questions
		const joined = await createClient(
			ClusterService,
			makeTransport(JOIN_TOKEN, {
				"X-Hotbed-Node-Id": "node-qbridge-worker",
				"X-Hotbed-Host": "qbridge-worker",
				"X-Hotbed-Addr": "100.73.156.77:7121",
			}),
		).join({});
		const workerWs = await SELF.fetch("https://controller.test/ws/questions", {
			headers: {
				Upgrade: "websocket",
				Authorization: `Bearer ${joined.workerToken}`,
			},
		});
		expect(workerWs.status).toBe(403);

		// 2. Seed a project and two sessions: one with t3codeThreadId, one without.
		await owner.cluster.addProject({
			name: "qbridge",
			repos: ["github.com/semistrict/qbridge"],
		});
		const t3Sess = await owner.sessions.startSession({
			id: "qbridge-t3",
			project: "qbridge",
			t3codeThreadId: "thread-bridge-1",
		});
		const plainSess = await owner.sessions.startSession({
			id: "qbridge-plain",
			project: "qbridge",
		});

		const promptA = create(PromptSchema, {
			question: "Which transport should we use?",
			header: "Transport",
			options: [
				{ label: "WebSocket", description: "Hibernating DO socket", implemented: true },
				{ label: "SSE", description: "Server-sent events" },
			],
		});
		const promptB = create(PromptSchema, {
			question: "Enable retries?",
			header: "Retries",
			options: [
				{ label: "Yes", description: "Retry on error", implemented: true },
				{ label: "No", description: "Fail fast" },
			],
		});

		// Add questions before connecting: q1 (T3, answered -> in initial snapshot),
		// q2 (T3, open -> NOT in snapshot), q3 (no T3 thread, answered -> NOT in snapshot).
		const addedT3 = await owner.questions.addQuestions({
			sessionId: t3Sess.id,
			questions: [promptA, promptB],
		});
		const q1 = addedT3.questions[0].id;
		const q2 = addedT3.questions[1].id;

		const addedPlain = await owner.questions.addQuestions({
			sessionId: plainSess.id,
			questions: [promptB],
		});
		const q3 = addedPlain.questions[0].id;

		await owner.questions.answerQuestion({
			id: q1,
			answer: { picked: ["WebSocket"], notes: "Keep it cheap while idle" },
		});
		await owner.questions.answerQuestion({
			id: q3,
			answer: { picked: ["Yes"] },
		});

		type BridgeMsg = {
			type: "snapshot" | "update";
			event?: string;
			questions?: Array<{
				id: number;
				state: string;
				revision?: string;
				t3codeThreadId?: string;
				answer?: { picked?: string[]; other?: string; notes?: string };
				dismissed?: { reason?: string };
			}>;
		};

		async function openBridge(): Promise<{
			ws: WebSocket;
			next: () => Promise<BridgeMsg | string>;
			close: () => void;
		}> {
			const res = await SELF.fetch("https://controller.test/ws/questions", {
				headers: {
					Upgrade: "websocket",
					Authorization: `Bearer ${OWNER_TOKEN}`,
					"X-Hotbed-Host": "macbook-bridge",
				},
			});
			expect(res.status).toBe(101);
			const ws = res.webSocket!;
			expect(ws).toBeTruthy();
			const queue: Array<BridgeMsg | string> = [];
			const waiters: Array<(v: BridgeMsg | string) => void> = [];
			ws.addEventListener("message", (ev) => {
				const raw = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data);
				const item: BridgeMsg | string = raw === "pong" ? "pong" : (JSON.parse(raw) as BridgeMsg);
				const w = waiters.shift();
				if (w) w(item);
				else queue.push(item);
			});
			ws.accept();
			return {
				ws,
				next: () =>
					new Promise((resolve, reject) => {
						if (queue.length > 0) {
							resolve(queue.shift()!);
							return;
						}
						const timer = setTimeout(
							() => reject(new Error("timed out waiting for bridge frame")),
							5000,
						);
						waiters.push((item) => {
							clearTimeout(timer);
							resolve(item);
						});
					}),
				close: () => {
					try {
						ws.close(1000, "test done");
					} catch {
						// ignore
					}
				},
			};
		}

		const bridge = await openBridge();
		try {
			// Initial snapshot contains only q1 (answered + has t3codeThreadId).
			const snap = (await bridge.next()) as BridgeMsg;
			expect(snap.type).toBe("snapshot");
			expect(snap.questions?.map((q) => q.id)).toEqual([q1]);
			expect(snap.questions?.[0].t3codeThreadId).toBe("thread-bridge-1");
			expect(snap.questions?.[0].state).toBe("QUESTION_STATE_ANSWERED");
			expect(snap.questions?.[0].answer?.picked).toEqual(["WebSocket"]);

			// Bridge socket must not inflate connectedWorkers in /healthz.
			const health = (await (await SELF.fetch("https://controller.test/healthz")).json()) as {
				connectedWorkers: number;
			};
			expect(health.connectedWorkers).toBe(0);

			// Keepalive ping -> pong
			bridge.ws.send("ping");
			expect(await bridge.next()).toBe("pong");

			// Mutating non-T3 question q3 produces no bridge update; follow with T3 q2 answer to verify ordering.
			await owner.questions.dismissQuestion({ id: q3, reason: "no t3 thread" });
			await owner.questions.answerQuestion({
				id: q2,
				answer: { picked: ["Yes"], other: "with jitter" },
			});
			const ansEv = (await bridge.next()) as BridgeMsg;
			expect(ansEv.type).toBe("update");
			expect(ansEv.event).toBe("answered");
			expect(ansEv.questions?.map((q) => q.id)).toEqual([q2]);
			expect(ansEv.questions?.[0].answer?.other).toBe("with jitter");

			// reviseQuestion pushes "revised" with incremented revision.
			const prevRev = Number(ansEv.questions?.[0].revision ?? "0");
			await owner.questions.reviseQuestion({
				id: q2,
				prompt: create(PromptSchema, {
					question: "Enable retries with exponential backoff?",
					header: "Retries",
					options: [
						{ label: "Yes", description: "Retry on error", implemented: true },
						{ label: "No", description: "Fail fast" },
					],
				}),
			});
			const revEv = (await bridge.next()) as BridgeMsg;
			expect(revEv.type).toBe("update");
			expect(revEv.event).toBe("revised");
			expect(revEv.questions?.[0].id).toBe(q2);
			expect(Number(revEv.questions?.[0].revision ?? "0")).toBeGreaterThan(prevRev);

			// dismissQuestion pushes "dismissed".
			await owner.questions.dismissQuestion({ id: q2, reason: "not needed anymore" });
			const disEv = (await bridge.next()) as BridgeMsg;
			expect(disEv.type).toBe("update");
			expect(disEv.event).toBe("dismissed");
			expect(disEv.questions?.[0].state).toBe("QUESTION_STATE_DISMISSED");
			expect(disEv.questions?.[0].dismissed?.reason).toBe("not needed anymore");

			// reopenQuestion pushes "reopened" with OPEN state.
			await owner.questions.reopenQuestion({ id: q2 });
			const reopenEv = (await bridge.next()) as BridgeMsg;
			expect(reopenEv.type).toBe("update");
			expect(reopenEv.event).toBe("reopened");
			expect(reopenEv.questions?.[0].state).toBe("QUESTION_STATE_OPEN");

			// ackQuestions pushes "handled" with HANDLED state.
			await owner.questions.ackQuestions({ sessionId: t3Sess.id, ids: [q1] });
			const ackEv = (await bridge.next()) as BridgeMsg;
			expect(ackEv.type).toBe("update");
			expect(ackEv.event).toBe("handled");
			expect(ackEv.questions?.[0].id).toBe(q1);
			expect(ackEv.questions?.[0].state).toBe("QUESTION_STATE_HANDLED");

			// deleteQuestion pushes "deleted".
			await owner.questions.deleteQuestion({
				sessionId: t3Sess.id,
				id: q2,
				reason: "obsolete",
			});
			const delEv = (await bridge.next()) as BridgeMsg;
			expect(delEv.type).toBe("update");
			expect(delEv.event).toBe("deleted");
			expect(delEv.questions?.[0].id).toBe(q2);
			expect(delEv.questions?.[0].state).toBe("QUESTION_STATE_DELETED");

			// Explicit resync returns an empty snapshot now that q1 is handled and q2 is deleted.
			bridge.ws.send(JSON.stringify({ type: "resync" }));
			const resyncSnap = (await bridge.next()) as BridgeMsg;
			expect(resyncSnap.type).toBe("snapshot");
			expect(resyncSnap.questions).toEqual([]);
		} finally {
			bridge.close();
		}
	});

	it("includes live freshen commands in getSnapshot and excludes ended ones", async () => {
		const owner = ownerClients();
		const joiner = workerClients(
			JOIN_TOKEN,
			"node-freshen-snap",
			"ubu-freshen",
			"100.73.156.99:7121",
		);
		const joined = await joiner.cluster.join({});
		const worker = workerClients(
			OWNER_TOKEN,
			"node-freshen-snap",
			"ubu-freshen",
			"100.73.156.99:7121",
		);

		await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					version: "build 191",
				}),
			}),
		);

		const nowMs = BigInt(Date.now());
		await worker.cluster.pushEvents({
			clusterId: joined.clusterId,
			epoch: "ep-freshen-snap",
			checkpoint: create(CheckpointSchema, {
				seq: 1n,
				commands: [
					create(CommandSchema, {
						id: "f-cladefs-vm-live",
						nodeId: "node-freshen-snap",
						project: "cladefs",
						kind: "freshen",
						runtime: "vm",
						argv: "freshen cladefs-vm-12345678",
						state: CommandState.RUNNING,
						queuedAt: nowMs,
						startedAt: nowMs,
					}),
					create(CommandSchema, {
						id: "f-cladefs-vm-done",
						nodeId: "node-freshen-snap",
						project: "cladefs",
						kind: "freshen",
						runtime: "vm",
						argv: "freshen cladefs-vm-00000000",
						state: CommandState.SUCCEEDED,
						queuedAt: nowMs - 1000n,
						startedAt: nowMs - 1000n,
						endedAt: nowMs,
					}),
				],
			}),
		});

		const snap = await owner.cluster.getSnapshot({});
		const liveFreshen = snap.commands.find((c) => c.id === "f-cladefs-vm-live");
		expect(liveFreshen?.runtime).toBe("vm");
		const ids = snap.commands.map((c) => c.id);
		expect(ids).toContain("f-cladefs-vm-live");
		expect(ids).not.toContain("f-cladefs-vm-done");
	});

	it("waits briefly for a reconnecting worker WebSocket tunnel instead of failing immediately", async () => {
		const owner = ownerClients();
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "proj-reconnect",
				url: "https://example.com/proj-reconnect.git",
				branch: "main",
			}),
		});
		const joiner = workerClients(JOIN_TOKEN, "node-reconn", "ubu-reconn", "100.73.156.81:7121");
		const joined = await joiner.cluster.join({});
		const worker = workerClients(OWNER_TOKEN, "node-reconn", "ubu-reconn", "100.73.156.81:7121");
		await worker.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(32 * 1024 ** 3),
					version: "build 195",
					runtimes: ["vm"],
				}),
			}),
		);
		const sess = await owner.sessions.startSession({
			id: "reconn-fox",
			project: "proj-reconnect",
			agent: "test",
		});

		// Start CreateWorkspace while the worker has no active tunnel yet; connect 150ms later.
		const tunnels: Array<Awaited<ReturnType<typeof openMockWorkerTunnel>>> = [];
		const connectTimer = setTimeout(async () => {
			tunnels.push(
				await openMockWorkerTunnel(OWNER_TOKEN, "node-reconn", "ubu-reconn", "100.73.156.81:7121", {
					unary: {
						"/hotbed.v1.WorkspaceService/CreateWorkspace": (reqBytes) => {
							const req = fromBinary(CreateWorkspaceRequestSchema, reqBytes);
							return toBinary(
								WorkspaceSchema,
								create(WorkspaceSchema, {
									id: req.id,
									sessionId: req.sessionId,
									project: "proj-reconnect",
									state: WorkspaceState.IDLE,
									node: "ubu-reconn",
								}),
							);
						},
					},
				}),
			);
		}, 150);

		try {
			const ws = await owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "proj-reconnect",
				placement: { node: "ubu-reconn" },
			});
			expect(ws.state).toBe(WorkspaceState.IDLE);
			expect(ws.node).toBe("ubu-reconn");
		} finally {
			clearTimeout(connectTimer);
			for (const t of tunnels) t.close();
		}
	});

	it("prefers an online node with an active WebSocket tunnel over one reconnecting", async () => {
		const owner = ownerClients();
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "proj-tunnel-pref",
				url: "https://example.com/proj-tunnel-pref.git",
				branch: "main",
			}),
		});
		const j1 = workerClients(JOIN_TOKEN, "node-tpref-1", "ubu-tpref-1", "100.73.156.82:7121");
		const joined1 = await j1.cluster.join({});
		const w1 = workerClients(OWNER_TOKEN, "node-tpref-1", "ubu-tpref-1", "100.73.156.82:7121");
		await w1.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined1.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 32,
					memoryBytes: BigInt(64 * 1024 ** 3),
					version: "build 195",
					runtimes: ["vm"],
				}),
			}),
		);

		const j2 = workerClients(JOIN_TOKEN, "node-tpref-2", "vm-tpref-2", "100.73.156.83:7121");
		const joined2 = await j2.cluster.join({});
		const w2 = workerClients(OWNER_TOKEN, "node-tpref-2", "vm-tpref-2", "100.73.156.83:7121");
		await w2.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined2.clusterId,
				resources: create(ResourcesSchema, {
					arch: "arm64",
					cpus: 4,
					memoryBytes: BigInt(8 * 1024 ** 3),
					version: "build 195",
					runtimes: ["cladefs"],
				}),
			}),
		);

		const tunnel2 = await openMockWorkerTunnel(
			OWNER_TOKEN,
			"node-tpref-2",
			"vm-tpref-2",
			"100.73.156.83:7121",
			{
				unary: {
					"/hotbed.v1.WorkspaceService/CreateWorkspace": (reqBytes) => {
						const req = fromBinary(CreateWorkspaceRequestSchema, reqBytes);
						return toBinary(
							WorkspaceSchema,
							create(WorkspaceSchema, {
								id: req.id,
								sessionId: req.sessionId,
								project: "proj-tunnel-pref",
								state: WorkspaceState.IDLE,
								node: "vm-tpref-2",
							}),
						);
					},
				},
			},
		);

		try {
			const sess = await owner.sessions.startSession({
				id: "tpref-owl",
				project: "proj-tunnel-pref",
				agent: "test",
			});
			const ws = await owner.workspaces.createWorkspace({
				sessionId: sess.id,
				repo: "proj-tunnel-pref",
			});
			// ubu-tpref-1 has no active tunnel (even though it has 32 CPUs & 64 GiB RAM),
			// so placement must immediately route to vm-tpref-2 which has a live tunnel.
			expect(ws.node).toBe("vm-tpref-2");
			expect(tunnel2.calls).toContain("/hotbed.v1.WorkspaceService/CreateWorkspace");
		} finally {
			tunnel2.close();
		}
	});

	it("persists base freshens across collection, aggregates phase/step latency, and enriches live queued freshens", async () => {
		const owner = ownerClients();
		await owner.cluster.putRepo({
			repo: create(RepoSchema, {
				id: "proj-freshen-obs",
				url: "https://example.com/proj-freshen-obs.git",
				branch: "main",
			}),
		});
		const j = workerClients(JOIN_TOKEN, "node-fobs-1", "ubu-fobs", "100.73.156.90:7121");
		const joined = await j.cluster.join({});
		const w = workerClients(OWNER_TOKEN, "node-fobs-1", "ubu-fobs", "100.73.156.90:7121");

		const now = Date.now();
		const t0 = BigInt(now - 600_000);
		const tAdmit = BigInt(now - 540_000); // 60s queued
		const tPark = BigInt(now - 400_000);
		const tThaw = BigInt(now - 340_000); // 60s parked
		const tDone = BigInt(now - 100_000);

		// Push a cold build g1 that succeeds, then gets collected from node_state,
		// plus a live queued g2 with a waiting workspace and node admission hold.
		const baseG1 = create(BaseSchema, {
			id: "proj-freshen-obs-g1",
			project: "proj-freshen-obs",
			nodeId: "node-fobs-1",
			generation: 1,
			runtime: "cladefs",
			state: BaseState.WARM,
			cold: true,
			coldReason: "base_image_changed",
			createdAt: t0,
			builtAt: tDone,
			buildSeconds: 440,
			prepSeconds: 12,
			gitSeconds: 18,
			finalizeSeconds: 4,
			peakMemoryBytes: BigInt(6 * 1024 ** 3),
			command: "f-proj-freshen-obs-g1-aaa111",
			steps: [
				create(AppliedStepSchema, { name: "deps", hash: "h1", ran: true, ok: true, seconds: 45 }),
				create(AppliedStepSchema, { name: "build", hash: "h2", ran: true, ok: true, seconds: 310 }),
			],
			phases: [
				create(FreshenPhaseSchema, { name: "clone", startedAt: t0, seconds: 12 }),
				create(FreshenPhaseSchema, { name: "snapshot", startedAt: tDone - 4000n, seconds: 4 }),
			],
		});
		const cmdG1 = create(CommandSchema, {
			id: "f-proj-freshen-obs-g1-aaa111",
			nodeId: "node-fobs-1",
			project: "proj-freshen-obs",
			argv: "freshen proj-freshen-obs g1",
			kind: "freshen",
			state: CommandState.SUCCEEDED,
			queuedAt: t0,
			startedAt: tAdmit,
			endedAt: tDone - 4000n,
			limits: create(CommandLimitsSchema, { memoryLowBytes: BigInt(5 * 1024 ** 3) }),
			timeline: [
				create(CommandEventSchema, { at: t0, what: "queued", detail: "base proj-freshen-obs-g1" }),
				create(CommandEventSchema, { at: tAdmit, what: "admitted" }),
				create(CommandEventSchema, { at: tPark, what: "parked", detail: "memory pressure" }),
				create(CommandEventSchema, { at: tThaw, what: "thawed" }),
				create(CommandEventSchema, { at: tDone - 4000n, what: "exited", detail: "exit 0" }),
			],
		});

		const tQ = BigInt(now - 120_000);
		const baseG2 = create(BaseSchema, {
			id: "proj-freshen-obs-g2",
			project: "proj-freshen-obs",
			nodeId: "node-fobs-1",
			generation: 2,
			runtime: "cladefs",
			state: BaseState.BUILDING,
			cold: false,
			fromBaseId: "proj-freshen-obs-g1",
			createdAt: tQ,
			command: "f-proj-freshen-obs-g2-bbb222",
		});
		const cmdG2 = create(CommandSchema, {
			id: "f-proj-freshen-obs-g2-bbb222",
			nodeId: "node-fobs-1",
			project: "proj-freshen-obs",
			argv: "freshen proj-freshen-obs g2",
			kind: "freshen",
			state: CommandState.QUEUED,
			queuedAt: tQ,
			reason: "waiting for memory: needs 6.0 GiB, 2.0 GiB left",
			limits: create(CommandLimitsSchema, { memoryLowBytes: BigInt(6 * 1024 ** 3) }),
			timeline: [
				create(CommandEventSchema, { at: tQ, what: "queued", detail: "base proj-freshen-obs-g2" }),
			],
		});
		const wsWaiting = create(WorkspaceSchema, {
			id: "ws-wait-g2",
			nodeId: "node-fobs-1",
			project: "proj-freshen-obs",
			baseId: "proj-freshen-obs-g2",
			state: WorkspaceState.PREPARING,
			createdAt: tQ,
		});

		await w.cluster.pushEvents(
			create(PushEventsRequestSchema, {
				clusterId: joined.clusterId,
				epoch: "ep-fobs-1",
				checkpoint: create(CheckpointSchema, { seq: 0n }),
			}),
		);
		await w.cluster.pushEvents(
			create(PushEventsRequestSchema, {
				clusterId: joined.clusterId,
				epoch: "ep-fobs-1",
				events: [
					create(EventSchema, {
						seq: 1n,
						at: tDone,
						change: {
							case: "command",
							value: create(CommandChangeSchema, { what: "exited", command: cmdG1 }),
						},
					}),
					create(EventSchema, {
						seq: 2n,
						at: tDone,
						change: {
							case: "base",
							value: create(BaseChangeSchema, { what: "warm", base: baseG1 }),
						},
					}),
					// Collect g1 and forget its command: base_freshens MUST retain it!
					create(EventSchema, {
						seq: 3n,
						at: tDone + 1000n,
						change: {
							case: "base",
							value: create(BaseChangeSchema, { what: "collected", base: baseG1 }),
						},
					}),
					create(EventSchema, {
						seq: 4n,
						at: tDone + 1000n,
						change: {
							case: "command",
							value: create(CommandChangeSchema, { what: "forgotten", command: cmdG1 }),
						},
					}),
					create(EventSchema, {
						seq: 5n,
						at: tQ,
						change: {
							case: "base",
							value: create(BaseChangeSchema, { what: "building", base: baseG2 }),
						},
					}),
					create(EventSchema, {
						seq: 6n,
						at: tQ,
						change: {
							case: "command",
							value: create(CommandChangeSchema, { what: "queued", command: cmdG2 }),
						},
					}),
					create(EventSchema, {
						seq: 7n,
						at: tQ,
						change: {
							case: "workspace",
							value: { what: "created", workspace: wsWaiting },
						},
					}),
				],
			}),
		);

		await w.cluster.heartbeat(
			create(HeartbeatRequestSchema, {
				clusterId: joined.clusterId,
				resources: create(ResourcesSchema, {
					arch: "amd64",
					cpus: 16,
					memoryBytes: BigInt(16 * 1024 ** 3),
				}),
				admission: create(AdmissionSchema, {
					holding: true,
					reason: "memory reserved 14.0 GiB",
					signals: [
						create(AdmissionSignalSchema, {
							name: "memory-reserved",
							value: 14 * 1024 ** 3,
							limit: 13.8 * 1024 ** 3,
							unit: "bytes",
							holding: true,
							waiting: 1,
						}),
					],
					queued: 1,
				}),
				commands: [
					create(CommandUsageSchema, {
						id: "f-proj-freshen-obs-g2-bbb222",
						reason: "waiting for memory: needs 6.0 GiB, 1.5 GiB left",
					}),
				],
			}),
		);

		const list = await owner.cluster.listFreshens({ project: "proj-freshen-obs" });
		expect(list.freshens).toHaveLength(2);
		// Active queued freshen g2 sorts first.
		expect(list.freshens[0].id).toBe("f-proj-freshen-obs-g2-bbb222");
		expect(list.freshens[0].state).toBe(FreshenState.QUEUED);
		expect(list.freshens[0].waitingWorkspaces).toEqual(["ws-wait-g2"]);
		expect(list.freshens[0].reason).toContain("needs 6.0 GiB, 1.5 GiB left");
		expect(list.freshens[0].queueSeconds).toBeGreaterThanOrEqual(100);

		// Collected g1 is still preserved with full phase, step, queue, and parked telemetry.
		expect(list.freshens[1].id).toBe("f-proj-freshen-obs-g1-aaa111");
		expect(list.freshens[1].state).toBe(FreshenState.SUCCEEDED);
		expect(list.freshens[1].coldReason).toBe("base_image_changed");
		expect(list.freshens[1].queueSeconds).toBe(60);
		expect(list.freshens[1].parks).toBe(1);
		expect(list.freshens[1].parkedSeconds).toBe(60);
		expect(list.freshens[1].gitSeconds).toBe(18);
		expect(list.freshens[1].finalizeSeconds).toBe(4);

		// Summary aggregates slowest steps and node/project rollup.
		expect(list.summary?.active).toBe(1);
		expect(list.summary?.queued).toBe(1);
		expect(list.summary?.succeeded).toBe(1);
		expect(list.summary?.waitingWorkspaces).toBe(1);
		expect(list.summary?.slowestSteps[0].step).toBe("build");
		expect(list.summary?.slowestSteps[0].maxSeconds).toBe(310);
		expect(list.summary?.byNodeProject[0].coldReasons.base_image_changed).toBe(1);

		// GetFreshen returns live admission snapshot for active g2.
		const detail = await owner.cluster.getFreshen({ id: "f-proj-freshen-obs-g2-bbb222" });
		expect(detail.freshen?.baseId).toBe("proj-freshen-obs-g2");
		expect(detail.nodeAdmission?.holding).toBe(true);
	});

	it("treats macOS warn (50%) host memory pressure as soft placement penalty and holds only on critical (100%)", () => {
		const warnUsage = create(HostUsageSchema, {
			vmCpuBusy: 0.1,
			hostCpuBusy: 0.25,
			hostLoad1m: 2.0,
			hostCpus: 10,
			hostMemoryPressure: 50,
		});
		const warnStarv = hostStarvation(undefined, warnUsage);
		expect(warnStarv.holding).toBe(false);
		const warnSig = warnStarv.signals.find((s) => s.name === "host-memory-pressure");
		expect(warnSig?.holding).toBe(false);
		expect(warnSig?.limit).toBe(100);
		expect(warnSig?.detail).toContain("warn");

		const warnCost = hostStarvationCost(0.1, warnUsage);
		expect(warnCost.value).toBeGreaterThan(0);
		expect(warnCost.value).toBeLessThan(1);
		expect(warnCost.detail).toContain("50%");

		const critUsage = create(HostUsageSchema, {
			vmCpuBusy: 0.1,
			hostCpuBusy: 0.25,
			hostLoad1m: 2.0,
			hostCpus: 10,
			hostMemoryPressure: 100,
		});
		const critStarv = hostStarvation(undefined, critUsage);
		expect(critStarv.holding).toBe(true);
		const critSig = critStarv.signals.find((s) => s.name === "host-memory-pressure");
		expect(critSig?.holding).toBe(true);
		expect(critSig?.detail).toContain("critical");
	});
});
