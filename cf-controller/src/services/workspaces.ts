import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create } from "@bufbuild/protobuf";
import {
	BaseState,
	CommandInterventionSchema,
	CommandLogSchema,
	CommandProfileSchema,
	CommandSchema,
	CommandState,
	CreateWorkspaceRequestSchema,
	DaemonLogSourceSchema,
	FileContentSchema,
	GetCommandRequestSchema,
	GetDaemonLogsRequestSchema,
	GetDaemonLogsResponseSchema,
	GetFileRequestSchema,
	GetLogLinesRequestSchema,
	GetProfileRequestSchema,
	GetWorkspaceRequestSchema,
	InspectCommandRequestSchema,
	InspectCommandResponseSchema,
	ListCommandsResponseSchema,
	ListFilesRequestSchema,
	ListFilesResponseSchema,
	ListWorkspacesResponseSchema,
	OpenShellRequestSchema,
	OpenShellResponseSchema,
	PauseCommandRequestSchema,
	RebuiltSchema,
	ReleaseWorkspaceRequestSchema,
	ReleaseWorkspaceResponseSchema,
	ResumeCommandRequestSchema,
	RunCommandRequestSchema,
	SampleCommandChunkSchema,
	SampleCommandRequestSchema,
	StopCommandRequestSchema,
	TraceCommandChunkSchema,
	TraceCommandRequestSchema,
	WorkspaceSchema,
	WorkspaceService,
	WorkspaceState,
	type Base,
	type Command,
	type CreateWorkspaceRequest,
	type DaemonLogEntry,
	type DaemonLogSource,
	type FinishedCommand,
	type Node,
	type RunCommandRequest,
	type Session,
	type StopCommandRequest,
	type Workspace,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { WORKER_RPC_TIMEOUT_MS, type ControllerContext } from "../context.ts";
import { ErrorCode, codeOf, fromWorkerError, hotbedError } from "../errors.ts";
import { newId } from "../gitops.ts";
import {
	buildCandidates,
	buildPlacementDecision,
	chooseCandidate,
	upgradingFor,
	type Candidate,
	type NodeView,
} from "../placement.ts";
import { BOOT_TIMEOUT_MS, isCommandLive } from "../standby.ts";
import { queryProfile } from "../profile-query.ts";
import { sessionEndedError, type CommandRoute, type WorkspaceRow } from "../store.ts";

const DEFAULT_COMMAND_LIMIT = 50;
const DIAGNOSE_RPC_TIMEOUT_MS = 90_000;
const WORKER_PROBE_TIMEOUT_MS = 5_000;
const DAEMON_LOG_WORKER_TIMEOUT_MS = 5_000;
const DEFAULT_DAEMON_LOG_LIMIT = 200;
const MAX_DAEMON_LOG_LIMIT = 1000;
const UPGRADE_WAIT_MS = 90_000;

function passOver(err: unknown): boolean {
	const c = codeOf(err);
	return c === ErrorCode.WorkerUnreachable || c === ErrorCode.RepoUnreachable;
}

function isUnreachable(err: unknown): boolean {
	return codeOf(err) === ErrorCode.WorkerUnreachable;
}

function commandOfHistory(f: FinishedCommand): Command {
	const ended = f.endedAt;
	const started = ended - BigInt(Math.round(f.seconds * 1000));
	const queuedAt = started - BigInt(Math.round(f.queuedSeconds * 1000));
	return create(CommandSchema, {
		id: f.id,
		workspaceId: f.workspaceId,
		nodeId: f.nodeId,
		argv: f.argv,
		kind: f.kind,
		state: f.state,
		queuedAt,
		startedAt: started,
		endedAt: ended,
		exitCode: f.exitCode,
		coldSeconds: f.coldSeconds,
		reason: f.reason,
		sessionId: f.sessionId,
		bench: f.bench,
		project: f.project,
		repo: f.repoId,
		runtime: f.runtime,
		rebuilt:
			f.rebuilt !== undefined && f.of !== undefined
				? create(RebuiltSchema, { units: f.rebuilt, of: f.of })
				: undefined,
		interventions: f.interventions.map((iv) => clone(CommandInterventionSchema, iv)),
	});
}

function parseDurationMs(raw: string): number {
	const s = raw.trim();
	if (!s) return 0;
	const m = /^(\d+)(ms|s|m|h|d)$/.exec(s);
	if (!m) {
		throw hotbedError(ErrorCode.InvalidArgs, `invalid duration "${raw}"`).withSuggestion(
			"use e.g. 15m, 1h, 24h, 7d",
		);
	}
	const n = Number(m[1]);
	switch (m[2]) {
		case "ms":
			return n;
		case "s":
			return n * 1000;
		case "m":
			return n * 60_000;
		case "h":
			return n * 3600_000;
		case "d":
			return n * 86_400_000;
		default:
			return 0;
	}
}

export interface WorkspaceHelpers {
	createWorkspaceInternal(
		req: CreateWorkspaceRequest,
		syncHost: string,
		signal?: AbortSignal,
	): Promise<Workspace>;
	createAwake(req: CreateWorkspaceRequest, signal?: AbortSignal): Promise<Workspace>;
	runCommandInternal(req: RunCommandRequest): Promise<Command>;
	stopCommandInternal(req: StopCommandRequest): Promise<Command>;
}

export function createWorkspaceService(ctrl: ControllerContext): {
	impl: ServiceImpl<typeof WorkspaceService>;
	helpers: WorkspaceHelpers;
} {
	function ensureOnlineWorker(nodeId: string): { host: string } {
		const row = ctrl.store.getNode(nodeId);
		const node = ctrl.withOnline(row.node);
		if (node.asleep) {
			throw hotbedError(ErrorCode.WorkerUnreachable, `${row.node.hostname} is asleep`, {
				retryable: true,
				suggestion: "a run placed on it wakes it",
			});
		}
		if (!node.online) {
			throw hotbedError(ErrorCode.WorkerUnreachable, `${row.node.hostname} is offline`, {
				retryable: true,
			});
		}
		return { host: row.node.hostname };
	}

	function workspaceOf(id: string): { host: string; row: WorkspaceRow; project: string } {
		const row = ctrl.store.getWorkspace(id);
		if (row.released) {
			throw hotbedError(ErrorCode.NotFound, `workspace ${id} was released`);
		}
		const sess = ctrl.store.liveSession(row.session, ctrl.now());
		const { host } = ensureOnlineWorker(row.node);
		return { host, row, project: sess.project };
	}

	function commandOf(id: string): { host: string; nodeId: string; session: string } {
		const route = ctrl.store.lookupCommand(id);
		if (route.session) ctrl.store.touchSession(route.session, ctrl.now());
		const { host } = ensureOnlineWorker(route.node);
		return { host, nodeId: route.node, session: route.session };
	}

	function viewWorkspace(id: string): Workspace {
		for (const nv of ctrl.views()) {
			const ws = nv.workspaces.find((w) => w.id === id);
			if (ws) return ws;
		}
		throw hotbedError(ErrorCode.NotFound, `no workspace ${id}`);
	}

	function viewCommandById(id: string): Command | null {
		const vc = ctrl.store.viewCommand(id);
		if (!vc) return null;
		let row;
		try {
			row = ctrl.store.getNode(vc.nodeId);
		} catch {
			return null;
		}
		const cmd = vc.cmd;
		cmd.nodeAddr = ctrl.addrOf(row);
		if (cmd.sessionId) {
			const sess = ctrl.store.tryGetSession(cmd.sessionId);
			if (sess?.project) cmd.project = sess.project;
		}
		if (ctrl.withOnline(row.node).online && row.hb && isCommandLive(cmd.state)) {
			const u = row.hb.commands.find((x) => x.id === cmd.id);
			if (u) {
				cmd.cpuCores = u.cpuCores;
				cmd.memoryBytes = u.memoryBytes;
				cmd.swapBytes = u.swapBytes;
				cmd.droppedOutputBytes = u.droppedOutputBytes;
				if (cmd.state === CommandState.QUEUED && u.reason) cmd.reason = u.reason;
			}
		}
		return cmd;
	}

	function baseCommand(id: string, route: CommandRoute, wasUnreachable: boolean): Command | null {
		let nodeId = route.node;
		let b: Base | undefined = route.base;
		if (!route.baseChecked) {
			const found = ctrl.store.baseByCommand(id);
			if (!found) return null;
			nodeId = found.nodeId;
			b = found.base;
		}
		if (!b) return null;
		const nid = b.nodeId || nodeId;
		const baseRuntime = b.cladefsCell
			? b.id.includes("-vm-")
				? "vm"
				: b.id.includes("-fuse-")
					? "cladefs"
					: ""
			: "chroot";
		const cmd = create(CommandSchema, {
			id,
			nodeId: nid,
			nodeAddr: ctrl.nodeAddr(nid),
			project: b.project,
			kind: "freshen",
			runtime: baseRuntime,
			argv: b.cladefsCell ? `freshen ${b.id}` : `freshen ${b.project} g${b.generation}`,
			endedAt: b.builtAt,
		});
		const failedStep = b.steps.find((st) => st.ran && !st.ok);
		if (failedStep) {
			cmd.state = CommandState.FAILED;
			cmd.reason = `base ${b.id} failed at step ${failedStep.name}`;
			return cmd;
		}
		if (b.state === BaseState.WARM) {
			cmd.state = CommandState.SUCCEEDED;
		} else if (b.state === BaseState.STALE) {
			if (b.builtAt !== undefined && b.builtAt > 0n) {
				cmd.state = CommandState.SUCCEEDED;
			} else {
				cmd.state = CommandState.CANCELLED;
				cmd.reason = `base ${b.id} became stale without completing`;
			}
		} else if (b.state === BaseState.FAILED) {
			cmd.state = CommandState.FAILED;
			cmd.reason = `base ${b.id} failed to freshen`;
		} else {
			if (wasUnreachable) return null;
			cmd.state = CommandState.FAILED;
			cmd.reason = `base ${b.id} freshen is no longer tracked by its worker`;
		}
		return cmd;
	}

	async function releaseFailedWorkspace(id: string): Promise<void> {
		try {
			await ctrl.releaseWorkspace(id);
		} catch (err) {
			const c = codeOf(err);
			if (c === ErrorCode.NotFound) return;
			if (c === ErrorCode.WorkerUnreachable) {
				ctrl.store.releaseWorkspaceRow(id, ctrl.now());
				return;
			}
			throw err;
		}
	}

	async function awaken(host: string, signal?: AbortSignal): Promise<void> {
		ctrl.power.wake(host, ctrl.now());
		const deadline = Date.now() + ctrl.power.wakeWaitMs;
		while (!signal?.aborted) {
			const waiter = ctrl.power.changes.wait(host);
			try {
				const n = ctrl.findNodeByHost(host);
				if (ctrl.nodeServes(n)) return;
				const rem = deadline - Date.now();
				if (rem <= 0) {
					throw hotbedError(ErrorCode.NodeWaking, `${host} was asleep and is still waking up`, {
						retryable: true,
						suggestion: "run again in a moment: hotbed run waits for it by itself",
					});
				}
				await waitPromise(waiter.promise, rem, signal);
			} finally {
				waiter.cancel();
			}
		}
		throw hotbedError(ErrorCode.Internal, "canceled while waking node");
	}

	async function waitUpgrade(
		hosts: string[],
		deadlineMs: number,
		signal?: AbortSignal,
	): Promise<boolean> {
		while (!signal?.aborted) {
			const waiters = hosts.map((h) => ctrl.power.changes.wait(h));
			try {
				const rows = ctrl.store.listNodes();
				if (rows.some((n) => hosts.includes(n.node.hostname) && ctrl.nodeServes(n))) {
					return true;
				}
				const rem = deadlineMs - Date.now();
				if (rem <= 0) return false;
				await waitPromise(Promise.race(waiters.map((w) => w.promise)), rem, signal);
			} finally {
				for (const w of waiters) w.cancel();
			}
		}
		return false;
	}

	async function getWorkspaceInternal(id: string): Promise<Workspace> {
		try {
			const { host, row, project } = workspaceOf(id);
			try {
				const ws = await ctrl.tunnel.callUnary(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/GetWorkspace",
					GetWorkspaceRequestSchema,
					create(GetWorkspaceRequestSchema, { id }),
					WorkspaceSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
				const nodeRow = ctrl.store.getNode(row.node);
				ws.nodeAddr = ctrl.addrOf(nodeRow);
				ws.node = nodeRow.node.hostname;
				ws.project = project;
				return ws;
			} catch (err) {
				const werr = fromWorkerError(host, err);
				if (isUnreachable(werr)) return viewWorkspace(id);
				throw werr;
			}
		} catch (err) {
			if (isUnreachable(err)) return viewWorkspace(id);
			throw err;
		}
	}

	async function createWorkspaceInternal(
		req: CreateWorkspaceRequest,
		syncHost: string,
		signal?: AbortSignal,
	): Promise<Workspace> {
		const sess = ctrl.store.liveSession(req.sessionId, ctrl.now());
		const skip = new Set<string>();
		let lastErr: unknown = null;
		let upgradeDeadline = 0;

		while (true) {
			const claimed = await claim(sess, req, syncHost, skip, signal);
			if (claimed.err) {
				if (claimed.upgrading.length > 0) {
					if (upgradeDeadline === 0) {
						upgradeDeadline =
							Date.now() + Math.min(UPGRADE_WAIT_MS, ctrl.power.wakeWaitMs || UPGRADE_WAIT_MS);
					}
					if (Date.now() < upgradeDeadline) {
						const ready = await waitUpgrade(claimed.upgrading, upgradeDeadline, signal);
						if (ready) continue;
					}
				}
				if (lastErr) throw lastErr;
				throw claimed.err;
			}
			const node = claimed.node!;
			if (node.asleep) {
				await awaken(node.host, signal);
				continue;
			}
			if (!claimed.fwd) {
				const inflight = ctrl.creatingWS.get(node.mine);
				if (inflight) await inflight.promise;
				try {
					const ws = await getWorkspaceInternal(node.mine);
					if (ws.state === WorkspaceState.FAILED) {
						await releaseFailedWorkspace(ws.id);
						continue;
					}
					if (ws.state === WorkspaceState.RELEASED) {
						ctrl.store.releaseWorkspaceRow(ws.id, ctrl.now());
						continue;
					}
					return ws;
				} catch (err) {
					if (codeOf(err) === ErrorCode.NotFound && !ctrl.creatingWS.has(node.mine)) {
						ctrl.store.reconcileMissingWorkspace(node.mine, ctrl.now());
						continue;
					}
					throw err;
				}
			}

			try {
				return await makeWorkspace(sess, node, claimed.fwd, signal);
			} catch (err) {
				if (signal?.aborted || !passOver(err)) throw err;
				skip.add(node.view.node.id);
				lastErr = err;
			}
		}
	}

	async function claim(
		sess: Session,
		req: CreateWorkspaceRequest,
		syncHost: string,
		skip: Set<string>,
		signal?: AbortSignal,
	): Promise<{
		node?: Candidate;
		fwd?: CreateWorkspaceRequest;
		upgrading: string[];
		err?: unknown;
	}> {
		const probed = new Set<string>();
		while (true) {
			const step = await ctrl.withPlacementLock(() => {
				const p = ctrl.store.getProject(sess.project);
				const hosted = ctrl.hosted(p);
				if (req.repo && !hosted.repos.some((r) => r.id === req.repo)) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`session ${sess.id} works on project ${p.name}, which holds no repo ${req.repo}`,
					).withSuggestion(
						`hotbed project add ${p.name} --repo REMOTE adds the repo's remote to it; or start a session in a project that holds it (hotbed session start -p PROJECT)`,
					);
				}
				const held = ctrl.store.sessionWorkspaces(sess.id);
				const nvs = ctrl.views();
				const needRelease: WorkspaceRow[] = [];
				const needProbe: WorkspaceRow[] = [];
				const mine = new Map<string, string>();
				for (const w of held) {
					const found = findWSInViews(nvs, w.id);
					if (found?.state === WorkspaceState.FAILED) {
						needRelease.push(w);
						continue;
					}
					if (!found && !ctrl.creatingWS.has(w.id) && !probed.has(w.id)) {
						needProbe.push(w);
						continue;
					}
					mine.set(w.node, w.id);
				}
				if (needRelease.length > 0 || needProbe.length > 0) {
					return { kind: "reconcile" as const, needRelease, needProbe };
				}
				const filtered = nvs.filter((nv) => !skip.has(nv.node.id));
				const cs = buildCandidates(
					filtered,
					p.name,
					mine,
					(arch) => ctrl.follows(arch),
					(arch) => ctrl.store.hasSharedBase(p.name, arch),
				);
				const { chosen, why, err } = chooseCandidate(
					cs,
					req.placement,
					p.hosts,
					req.syncDir !== "",
				);
				const d = buildPlacementDecision(cs, req.placement, p.hosts, chosen, why, err);
				d.sessionId = sess.id;
				d.project = p.name;
				d.repo = req.repo;
				ctrl.store.addPlacement(d, ctrl.now());
				if (err || !chosen) {
					const up = upgradingFor(cs, req.placement, p.hosts, req.syncDir !== "");
					return { kind: "done" as const, upgrading: up, err };
				}
				if (chosen.asleep) {
					return { kind: "done" as const, node: chosen, upgrading: [] };
				}
				ctrl.power.wake(chosen.host, ctrl.now());
				if (chosen.mine) {
					return { kind: "done" as const, node: chosen, upgrading: [] };
				}
				const chosenArch = chosen.view.hb?.resources?.arch ?? "";
				const chosenRuntimes = chosen.view.hb?.resources?.runtimes ?? [];
				const reqRuntime = req.placement?.bench ? "chroot" : (req.placement?.runtime ?? "");
				const usesCladefs =
					reqRuntime === "vm" ||
					reqRuntime === "cladefs" ||
					(reqRuntime === "" &&
						(chosenRuntimes.includes("vm") || chosenRuntimes.includes("cladefs")));
				const sharedBase = usesCladefs
					? ctrl.store.latestSharedBase(p.name, chosenArch)
					: undefined;
				const fencingToken = usesCladefs ? ctrl.store.nextFencingToken() : 0n;
				const fwd = create(CreateWorkspaceRequestSchema, {
					repo: req.repo,
					agent: sess.agent,
					id: newId(),
					sessionId: sess.id,
					syncDir: req.syncDir,
					syncHost: req.syncDir ? syncHost : "",
					project: hosted,
					runtime: reqRuntime,
					sharedBase,
					fencingToken,
				});
				const ok = ctrl.store.addLiveWorkspace(
					fwd.id,
					sess.id,
					chosen.view.node.id,
					sess.agent,
					ctrl.now(),
				);
				if (!ok) {
					const cur = ctrl.store.getSession(sess.id);
					throw sessionEndedError(sess.id, cur.endReason);
				}
				ctrl.markCreatingWS(fwd.id);
				return { kind: "done" as const, node: chosen, fwd, upgrading: [] };
			});

			if (step.kind === "done") {
				return step;
			}
			for (const w of step.needRelease) {
				await releaseFailedWorkspace(w.id);
			}
			for (const w of step.needProbe) {
				probed.add(w.id);
				try {
					const { host } = ensureOnlineWorker(w.node);
					const gws = await ctrl.tunnel.callUnary(
						w.node,
						host,
						"/hotbed.v1.WorkspaceService/GetWorkspace",
						GetWorkspaceRequestSchema,
						create(GetWorkspaceRequestSchema, { id: w.id }),
						WorkspaceSchema,
						WORKER_PROBE_TIMEOUT_MS,
					);
					if (gws.state === WorkspaceState.FAILED) {
						await releaseFailedWorkspace(w.id);
					} else if (gws.state === WorkspaceState.RELEASED) {
						ctrl.store.releaseWorkspaceRow(w.id, ctrl.now());
					}
				} catch (err) {
					if (codeOf(err) === ErrorCode.NotFound) {
						ctrl.store.reconcileMissingWorkspace(w.id, ctrl.now());
					}
				}
			}
			if (signal?.aborted) {
				throw hotbedError(ErrorCode.Internal, "placement canceled");
			}
		}
	}

	async function makeWorkspace(
		sess: Session,
		node: Candidate,
		fwd: CreateWorkspaceRequest,
		signal?: AbortSignal,
	): Promise<Workspace> {
		try {
			const { host } = ensureOnlineWorker(node.view.node.id);
			let ws: Workspace;
			try {
				ws = await ctrl.tunnel.callUnary(
					node.view.node.id,
					host,
					"/hotbed.v1.WorkspaceService/CreateWorkspace",
					CreateWorkspaceRequestSchema,
					fwd,
					WorkspaceSchema,
					120_000,
					signal,
				);
			} catch (err) {
				ctrl.store.dropWorkspace(fwd.id);
				throw fromWorkerError(node.host, err);
			}
			const wRow = ctrl.store.getWorkspace(fwd.id);
			const sessNow = ctrl.store.tryGetSession(sess.id);
			if (wRow.released || (sessNow && sessNow.endedAt !== undefined)) {
				try {
					await ctrl.tunnel.callUnary(
						node.view.node.id,
						host,
						"/hotbed.v1.WorkspaceService/ReleaseWorkspace",
						ReleaseWorkspaceRequestSchema,
						create(ReleaseWorkspaceRequestSchema, { id: fwd.id }),
						ReleaseWorkspaceResponseSchema,
						WORKER_RPC_TIMEOUT_MS,
					);
				} catch {
					// Worker's next heartbeat picks up the released workspace row via toRelease().
				}
				ctrl.store.releaseWorkspaceRow(fwd.id, ctrl.now());
				if (sessNow && sessNow.endedAt !== undefined) {
					throw sessionEndedError(sess.id, sessNow.endReason);
				}
				throw hotbedError(ErrorCode.NotFound, `workspace ${fwd.id} was released`);
			}
			ws.nodeAddr = ctrl.addrOf(node.view);
			ws.node = node.host;
			ws.project = sess.project;
			return ws;
		} finally {
			ctrl.unmarkCreatingWS(fwd.id);
		}
	}

	async function createAwake(
		req: CreateWorkspaceRequest,
		signal?: AbortSignal,
	): Promise<Workspace> {
		const deadline = Date.now() + BOOT_TIMEOUT_MS + ctrl.power.wakeWaitMs;
		while (true) {
			try {
				return await createWorkspaceInternal(req, "", signal);
			} catch (err) {
				if (codeOf(err) !== ErrorCode.NodeWaking || signal?.aborted || Date.now() > deadline) {
					throw err;
				}
			}
		}
	}

	async function runCommandInternal(req: RunCommandRequest): Promise<Command> {
		const { host, row, project } = workspaceOf(req.workspaceId);
		const fwd = create(RunCommandRequestSchema, {
			workspaceId: req.workspaceId,
			args: req.args,
			env: req.env,
			kind: req.kind,
			limits: req.limits,
			attached: req.attached,
			port: req.port,
			bench: req.bench,
			id: newId(),
			repo: req.repo,
		});
		ctrl.store.addCommand(fwd.id, row.session, row.node, row.id, ctrl.now());
		try {
			const cmd = await ctrl.tunnel.callUnary(
				row.node,
				host,
				"/hotbed.v1.WorkspaceService/RunCommand",
				RunCommandRequestSchema,
				fwd,
				CommandSchema,
				WORKER_RPC_TIMEOUT_MS,
			);
			cmd.project = project;
			return cmd;
		} catch (err) {
			ctrl.store.dropCommand(fwd.id);
			throw fromWorkerError(host, err);
		}
	}

	async function byHand(
		id: string,
		reqSession: string,
		diagnose: boolean,
		procedure: string,
		call: (nodeId: string, host: string, timeoutMs: number) => Promise<Command>,
	): Promise<Command> {
		const route = ctrl.store.lookupCommand(id);
		if (reqSession && reqSession !== route.session) {
			throw hotbedError(ErrorCode.NotFound, `session ${reqSession} has no command ${id}`);
		}
		if (route.session) ctrl.store.touchSession(route.session, ctrl.now());
		const { host } = ensureOnlineWorker(route.node);
		const timeout = diagnose ? DIAGNOSE_RPC_TIMEOUT_MS : WORKER_RPC_TIMEOUT_MS;
		try {
			const res = await call(route.node, host, timeout);
			if (route.session) {
				const sess = ctrl.store.tryGetSession(route.session);
				if (sess) res.project = sess.project;
			}
			return res;
		} catch (err) {
			throw fromWorkerError(host, err);
		}
	}

	async function stopCommandInternal(req: StopCommandRequest): Promise<Command> {
		const fwd = clone(StopCommandRequestSchema, req);
		fwd.host = "";
		fwd.internal = true;
		if (!fwd.reason) fwd.reason = "stopped by controller";
		return byHand(
			fwd.id,
			"",
			fwd.diagnose,
			"/hotbed.v1.WorkspaceService/StopCommand",
			(nodeId, host, t) =>
				ctrl.tunnel.callUnary(
					nodeId,
					host,
					"/hotbed.v1.WorkspaceService/StopCommand",
					StopCommandRequestSchema,
					fwd,
					CommandSchema,
					t,
				),
		);
	}

	const impl: ServiceImpl<typeof WorkspaceService> = {
		async createWorkspace(req, ctx) {
			const caller = getCaller(ctx.values);
			return createWorkspaceInternal(req, caller.hostname, ctx.signal);
		},

		async getWorkspace(req) {
			return getWorkspaceInternal(req.id);
		},

		watchWorkspace() {
			throw hotbedError(
				ErrorCode.PlatformNotSupported,
				"WatchWorkspace is served directly by the workspace's worker",
			);
		},

		async listWorkspaces(req) {
			const sess = req.sessionId ? ctrl.store.liveSession(req.sessionId, ctrl.now()) : null;
			if (req.project) ctrl.store.getProject(req.project);
			const nvs = ctrl.views();
			const viewIds: string[] = [];
			for (const nv of nvs) {
				for (const ws of nv.workspaces) {
					if (ws.state !== WorkspaceState.RELEASED) viewIds.push(ws.id);
				}
			}
			const releasedWS = ctrl.store.releasedWorkspaces(viewIds);
			const out: Workspace[] = [];
			const seen = new Set<string>();
			for (const nv of nvs) {
				for (const ws of nv.workspaces) {
					if (ws.state === WorkspaceState.RELEASED || releasedWS.has(ws.id)) continue;
					if (req.sessionId && ws.sessionId !== req.sessionId) continue;
					if (req.project && ws.project !== req.project) continue;
					out.push(ws);
					seen.add(ws.id);
				}
			}
			for (const id of sess?.workspaceIds ?? []) {
				if (seen.has(id)) continue;
				try {
					const ws = await getWorkspaceInternal(id);
					if (req.project && ws.project !== req.project) continue;
					out.push(ws);
				} catch (err) {
					if (codeOf(err) === ErrorCode.NotFound && !ctrl.creatingWS.has(id)) {
						ctrl.store.reconcileMissingWorkspace(id, ctrl.now());
					}
				}
			}
			out.sort((a, b) => Number(a.createdAt - b.createdAt));
			return create(ListWorkspacesResponseSchema, { workspaces: out });
		},

		async releaseWorkspace(req) {
			workspaceOf(req.id);
			await ctrl.releaseWorkspace(req.id);
			return create(ReleaseWorkspaceResponseSchema);
		},

		async sync() {
			throw hotbedError(
				ErrorCode.PlatformNotSupported,
				"Sync is served directly by the workspace's worker",
			);
		},

		async listFiles(req) {
			const { host, row } = workspaceOf(req.workspaceId);
			try {
				return await ctrl.tunnel.callUnary(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/ListFiles",
					ListFilesRequestSchema,
					req,
					ListFilesResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
		},

		async getFile(req) {
			const { host, row } = workspaceOf(req.workspaceId);
			try {
				return await ctrl.tunnel.callUnary(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/GetFile",
					GetFileRequestSchema,
					req,
					FileContentSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
		},

		async runCommand() {
			throw hotbedError(
				ErrorCode.PlatformNotSupported,
				"RunCommand is served directly by the workspace's worker",
			);
		},

		async openShell(req) {
			const { host, row } = workspaceOf(req.workspaceId);
			try {
				return await ctrl.tunnel.callUnary(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/OpenShell",
					OpenShellRequestSchema,
					req,
					OpenShellResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
		},

		async getCommand(req) {
			let route: CommandRoute | null = null;
			let err: unknown = null;
			try {
				route = ctrl.store.lookupCommand(req.id);
			} catch (e) {
				if (codeOf(e) !== ErrorCode.NotFound) throw e;
				err = e;
			}
			if (route) {
				if (route.session) ctrl.store.touchSession(route.session, ctrl.now());
				let host = route.node;
				try {
					host = ensureOnlineWorker(route.node).host;
					const cmd = await ctrl.tunnel.callUnary(
						route.node,
						host,
						"/hotbed.v1.WorkspaceService/GetCommand",
						GetCommandRequestSchema,
						req,
						CommandSchema,
						WORKER_RPC_TIMEOUT_MS,
					);
					cmd.nodeAddr = ctrl.nodeAddr(cmd.nodeId);
					if (route.session) {
						const sess = ctrl.store.tryGetSession(route.session);
						if (sess) cmd.project = sess.project;
					}
					return cmd;
				} catch (e) {
					err = fromWorkerError(host, e);
				}
			}

			if (isUnreachable(err)) {
				const vcmd = viewCommandById(req.id);
				if (vcmd) return vcmd;
				if (!route?.histChecked) {
					const f = ctrl.store.finishedCommand(req.id);
					if (f) {
						const c = commandOfHistory(f);
						c.nodeAddr = ctrl.nodeAddr(f.nodeId);
						return c;
					}
				}
				if (route) {
					const bc = baseCommand(req.id, route, true);
					if (bc) return bc;
				}
				throw err;
			}
			if (codeOf(err) === ErrorCode.NotFound) {
				if (!route?.histChecked) {
					const f = ctrl.store.finishedCommand(req.id);
					if (f) {
						const c = commandOfHistory(f);
						c.nodeAddr = ctrl.nodeAddr(f.nodeId);
						return c;
					}
				}
				const vcmd = viewCommandById(req.id);
				if (vcmd && !isCommandLive(vcmd.state)) return vcmd;
				if (route) {
					const bc = baseCommand(req.id, route, false);
					if (bc) return bc;
				}
				if (route?.node) {
					const sess = route.session ? ctrl.store.tryGetSession(route.session) : null;
					return create(CommandSchema, {
						id: req.id,
						nodeId: route.node,
						nodeAddr: ctrl.nodeAddr(route.node),
						sessionId: route.session,
						project: sess?.project ?? "",
						kind: !route.session && req.id.startsWith("f-") ? "freshen" : "",
						state: CommandState.FAILED,
						reason: "command is no longer tracked by its worker",
					});
				}
			}
			throw err;
		},

		async listCommands(req) {
			if (req.limit !== undefined && req.limit < 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`limit ${req.limit}: give 0 for every ended command, or a positive number`,
				);
			}
			let sess: Session | null = null;
			if (req.sessionId) {
				ctrl.store.touchSession(req.sessionId, ctrl.now());
				sess = ctrl.store.tryGetSession(req.sessionId);
			}
			if (req.project) ctrl.store.getProject(req.project);
			const nvs = ctrl.views();
			let targetNodes: Set<string> | null = null;
			if (req.sessionId) {
				targetNodes = new Set<string>();
				for (const sw of sess?.workspaces ?? []) targetNodes.add(sw.nodeId);
				for (const nv of nvs) {
					for (const ws of nv.workspaces)
						if (ws.sessionId === req.sessionId) targetNodes.add(nv.node.id);
					for (const c of nv.commands)
						if (c.sessionId === req.sessionId) targetNodes.add(nv.node.id);
				}
			}
			const live = await ctrl.liveNodeCommands(nvs, targetNodes, req.sessionId);
			const running: Command[] = [];
			const endedView: Command[] = [];
			for (const nv of nvs) {
				const cmds = live.get(nv.node.id) ?? nv.commands;
				for (const c of cmds) {
					if (req.sessionId && c.sessionId !== req.sessionId) continue;
					if (req.project && c.project !== req.project) continue;
					if (req.match && !c.argv.includes(req.match)) continue;
					if (c.endedAt === undefined) running.push(c);
					else endedView.push(c);
				}
			}
			const limit = req.limit !== undefined ? req.limit : DEFAULT_COMMAND_LIMIT;
			let picked = [...endedView];
			let total = endedView.length;
			if (req.ended) {
				const { items, total: kept } = ctrl.store.historyMatching(
					{ project: req.project, session: req.sessionId, match: req.match },
					limit,
				);
				const viewIds = endedView.map((c) => c.id);
				const inHist = ctrl.store.inHistory(viewIds);
				total = kept + viewIds.filter((id) => !inHist.has(id)).length;
				const seen = new Set(viewIds);
				for (const h of items) {
					if (!seen.has(h.id)) {
						const c = commandOfHistory(h);
						c.nodeAddr = ctrl.nodeAddr(h.nodeId);
						picked.push(c);
					}
				}
			}
			picked.sort((a, b) => {
				const ea = a.endedAt ?? 0n;
				const eb = b.endedAt ?? 0n;
				if (eb !== ea) return eb > ea ? 1 : -1;
				return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
			});
			if (limit > 0 && picked.length > limit) {
				picked = picked.slice(0, limit);
			}
			const all = [...running, ...picked];
			all.sort((a, b) => {
				if (a.queuedAt !== b.queuedAt) return a.queuedAt < b.queuedAt ? -1 : 1;
				return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
			});
			return create(ListCommandsResponseSchema, {
				commands: all,
				more: Math.max(0, total - picked.length),
			});
		},

		async stopCommand(req) {
			const fwd = clone(StopCommandRequestSchema, req);
			fwd.host = "";
			fwd.internal = false;
			fwd.reason = "";
			return byHand(
				fwd.id,
				fwd.sessionId,
				fwd.diagnose,
				"/hotbed.v1.WorkspaceService/StopCommand",
				(nodeId, host, t) =>
					ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/StopCommand",
						StopCommandRequestSchema,
						fwd,
						CommandSchema,
						t,
					),
			);
		},

		async pauseCommand(req) {
			const fwd = clone(PauseCommandRequestSchema, req);
			fwd.host = "";
			return byHand(
				fwd.id,
				fwd.sessionId,
				fwd.diagnose,
				"/hotbed.v1.WorkspaceService/PauseCommand",
				(nodeId, host, t) =>
					ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/PauseCommand",
						PauseCommandRequestSchema,
						fwd,
						CommandSchema,
						t,
					),
			);
		},

		async resumeCommand(req) {
			const fwd = clone(ResumeCommandRequestSchema, req);
			fwd.host = "";
			return byHand(
				fwd.id,
				fwd.sessionId,
				false,
				"/hotbed.v1.WorkspaceService/ResumeCommand",
				(nodeId, host, t) =>
					ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/ResumeCommand",
						ResumeCommandRequestSchema,
						fwd,
						CommandSchema,
						t,
					),
			);
		},

		followLog() {
			throw hotbedError(
				ErrorCode.PlatformNotSupported,
				"FollowLog is served directly by the workspace's worker",
			);
		},

		async getLogLines(req) {
			const { host, nodeId } = commandOf(req.id);
			try {
				return await ctrl.tunnel.callUnary(
					nodeId,
					host,
					"/hotbed.v1.WorkspaceService/GetLogLines",
					GetLogLinesRequestSchema,
					req,
					CommandLogSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
		},

		async inspectCommand(req) {
			const { host, nodeId } = commandOf(req.id);
			try {
				return await ctrl.tunnel.callUnary(
					nodeId,
					host,
					"/hotbed.v1.WorkspaceService/InspectCommand",
					InspectCommandRequestSchema,
					req,
					InspectCommandResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
		},

		async *traceCommand(req, ctx) {
			const { host, nodeId, session } = commandOf(req.id);
			let touched = Date.now();
			for await (const chunk of ctrl.tunnel.callServerStream(
				nodeId,
				host,
				"/hotbed.v1.WorkspaceService/TraceCommand",
				TraceCommandRequestSchema,
				req,
				TraceCommandChunkSchema,
				ctx.signal,
			)) {
				yield chunk;
				if (session && Date.now() - touched > 60_000) {
					touched = Date.now();
					ctrl.store.touchSession(session, ctrl.now());
				}
			}
		},

		async *sampleCommand(req, ctx) {
			const { host, nodeId, session } = commandOf(req.id);
			let touched = Date.now();
			for await (const chunk of ctrl.tunnel.callServerStream(
				nodeId,
				host,
				"/hotbed.v1.WorkspaceService/SampleCommand",
				SampleCommandRequestSchema,
				req,
				SampleCommandChunkSchema,
				ctx.signal,
			)) {
				yield chunk;
				if (session && Date.now() - touched > 60_000) {
					touched = Date.now();
					ctrl.store.touchSession(session, ctrl.now());
				}
			}
		},

		async getProfile(req) {
			if (req.target.case === "commandId") {
				const id = req.target.value;
				if (!id.trim() || id.trim() !== id) {
					throw hotbedError(ErrorCode.InvalidArgs, "valid command ID is required").withSuggestion(
						"pass a command ID (`hotbed profile CMD`)",
					);
				}
				const stored = ctrl.store.getCommandProfile(id);
				if (stored) {
					stored.live = false;
					return queryProfile(stored, req);
				}
				try {
					const { host, nodeId } = commandOf(id);
					return await ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/GetProfile",
						GetProfileRequestSchema,
						req,
						CommandProfileSchema,
						WORKER_RPC_TIMEOUT_MS,
					);
				} catch (err) {
					const f = ctrl.store.finishedCommand(id);
					if (f) {
						return create(CommandProfileSchema, {
							commandId: id,
							profiler: "opentelemetry-ebpf-profiler",
							live: false,
							active: false,
							statusNote: "no continuous eBPF samples were recorded for this command",
						});
					}
					throw err;
				}
			}
			if (req.target.case === "node") {
				const target = req.target.value.trim();
				if (!target) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						"node hostname or ID is required",
					).withSuggestion("pass a node name from `hotbed nodes`");
				}
				const n = ctrl.findNodeByNameOrId(target);
				const { host } = ensureOnlineWorker(n.node.id);
				const res = await ctrl.tunnel.callUnary(
					n.node.id,
					host,
					"/hotbed.v1.WorkspaceService/GetProfile",
					GetProfileRequestSchema,
					req,
					CommandProfileSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
				if (!res.node) res.node = host;
				return res;
			}
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"profile target (command_id or node) is required",
			).withSuggestion(
				"pass a command ID (`hotbed profile CMD`) or a node (`hotbed profile --node HOST`)",
			);
		},

		async getDaemonLogs(req) {
			const nowMs = ctrl.now();
			const limit =
				req.limit <= 0 ? DEFAULT_DAEMON_LOG_LIMIT : Math.min(req.limit, MAX_DAEMON_LOG_LIMIT);
			let fromMs = Number(req.fromMs);
			const toMs = Number(req.toMs) || nowMs;
			if (req.since && fromMs === 0) {
				fromMs = nowMs - parseDurationMs(req.since);
			}
			const normalized = create(GetDaemonLogsRequestSchema, {
				level: req.level,
				query: req.query,
				fromMs: BigInt(fromMs),
				toMs: BigInt(toMs),
				limit,
			});

			const target = req.source.trim();
			if (target === "controller") {
				return ctrl.store.queryDaemonLogs(
					ctrl.controllerName,
					req.level,
					req.query,
					fromMs,
					toMs,
					limit,
					nowMs,
				);
			}

			const rows = ctrl.store.listNodes();
			if (target) {
				const match = rows.find((r) => r.node.hostname === target || r.node.id === target);
				if (!match) {
					throw hotbedError(ErrorCode.NotFound, `no daemon log source "${target}"`).withSuggestion(
						`pass "controller" or a node name from hotbed nodes`,
					);
				}
				const { host } = ensureOnlineWorker(match.node.id);
				const res = await ctrl.tunnel.callUnary(
					match.node.id,
					host,
					"/hotbed.v1.WorkspaceService/GetDaemonLogs",
					GetDaemonLogsRequestSchema,
					normalized,
					GetDaemonLogsResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
				fillWorkerLogMetadata(res.entries, res.sources, host);
				return res;
			}

			const ctrlRes = ctrl.store.queryDaemonLogs(
				ctrl.controllerName,
				req.level,
				req.query,
				fromMs,
				toMs,
				limit,
				nowMs,
			);
			const workerResults = await Promise.all(
				rows.map(async (r) => {
					const node = ctrl.withOnline(r.node);
					if (!node.online || !ctrl.tunnel.isConnected(node.id)) {
						return {
							entries: [] as DaemonLogEntry[],
							source: unreachableDaemonLogSource(
								node,
								node.online ? "tunnel disconnected" : "offline",
							),
							truncated: false,
						};
					}
					try {
						const res = await ctrl.tunnel.callUnary(
							node.id,
							node.hostname,
							"/hotbed.v1.WorkspaceService/GetDaemonLogs",
							GetDaemonLogsRequestSchema,
							normalized,
							GetDaemonLogsResponseSchema,
							DAEMON_LOG_WORKER_TIMEOUT_MS,
						);
						fillWorkerLogMetadata(res.entries, res.sources, node.hostname);
						const src =
							res.sources[0] ??
							create(DaemonLogSourceSchema, {
								source: node.hostname,
								role: "worker",
								host: node.hostname,
								reachable: true,
							});
						return { entries: res.entries, source: src, truncated: res.truncated };
					} catch (err) {
						return {
							entries: [] as DaemonLogEntry[],
							source: unreachableDaemonLogSource(node, String(err)),
							truncated: false,
						};
					}
				}),
			);

			let allEntries: DaemonLogEntry[] = [...ctrlRes.entries];
			const sources: DaemonLogSource[] = [...ctrlRes.sources];
			let truncated = ctrlRes.truncated;
			for (const wr of workerResults) {
				allEntries.push(...wr.entries);
				sources.push(wr.source);
				if (wr.truncated) truncated = true;
			}
			allEntries.sort((a, b) => {
				if (b.at !== a.at) return b.at > a.at ? 1 : -1;
				if (a.source !== b.source) return a.source < b.source ? -1 : 1;
				return a.msg < b.msg ? -1 : a.msg > b.msg ? 1 : 0;
			});
			if (allEntries.length > limit) {
				allEntries = allEntries.slice(0, limit);
				truncated = true;
			}
			sources.sort((a, b) => {
				if (a.role !== b.role) {
					if (a.role === "controller") return -1;
					if (b.role === "controller") return 1;
				}
				return a.source < b.source ? -1 : a.source > b.source ? 1 : 0;
			});
			return create(GetDaemonLogsResponseSchema, { entries: allEntries, sources, truncated });
		},

		upgradeSystem() {
			throw hotbedError(
				ErrorCode.NotAuthorized,
				"WorkspaceService.UpgradeSystem is an internal controller-to-worker RPC",
			).withSuggestion("run `hotbed system upgrade` instead");
		},
	};

	return {
		impl,
		helpers: {
			createWorkspaceInternal,
			createAwake,
			runCommandInternal,
			stopCommandInternal,
		},
	};
}

function findWSInViews(nvs: NodeView[], id: string): Workspace | undefined {
	for (const nv of nvs) {
		const w = nv.workspaces.find((ws) => ws.id === id);
		if (w) return w;
	}
	return undefined;
}

function fillWorkerLogMetadata(
	entries: DaemonLogEntry[],
	sources: DaemonLogSource[],
	host: string,
): void {
	for (const e of entries) {
		if (!e.source) e.source = host;
		if (!e.role) e.role = "worker";
		if (!e.host) e.host = host;
	}
	for (const s of sources) {
		if (!s.source) s.source = host;
		if (!s.role) s.role = "worker";
		if (!s.host) s.host = host;
	}
}

function unreachableDaemonLogSource(n: Node, msg: string): DaemonLogSource {
	const prev = n.daemonLogs;
	return create(DaemonLogSourceSchema, {
		source: n.hostname,
		role: "worker",
		host: n.hostname,
		reachable: false,
		error: msg,
		errors24h: prev?.errors24h ?? 0,
		warns24h: prev?.warns24h ?? 0,
		lastAt: prev?.lastAt ?? 0n,
		diskBytes: prev?.diskBytes ?? 0n,
		files: prev?.files ?? 0,
		maxFileBytes: prev?.maxFileBytes ?? 0n,
		maxFiles: prev?.maxFiles ?? 0,
		maxAgeDays: prev?.maxAgeDays ?? 0,
	});
}

function waitPromise(p: Promise<void>, timeoutMs: number, signal?: AbortSignal): Promise<void> {
	return new Promise<void>((resolve) => {
		if (signal?.aborted) {
			resolve();
			return;
		}
		const done = () => {
			clearTimeout(timer);
			signal?.removeEventListener("abort", done);
			resolve();
		};
		const timer = setTimeout(done, timeoutMs);
		signal?.addEventListener("abort", done, { once: true });
		void p.then(done);
	});
}
