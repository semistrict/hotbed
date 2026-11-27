import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create, type MessageShape } from "@bufbuild/protobuf";
import {
	BaseState,
	CommandInterventionSchema,
	CommandLogSchema,
	CommandProfileSchema,
	CommandSchema,
	CommandEstimateSchema,
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
	FollowLogRequestSchema,
	LogChunkSchema,
	QueryOutputRequestSchema,
	OutputQuerySchema,
	DetachCommandRequestSchema,
	SendInputRequestSchema,
	SendInputResponseSchema,
	InspectCommandResponseSchema,
	ListCommandsResponseSchema,
	ListFilesRequestSchema,
	ListFilesResponseSchema,
	ListWorkspacesResponseSchema,
	OpenShellRequestSchema,
	OpenShellResponseSchema,
	PauseCommandRequestSchema,
	PlacementDecisionSchema,
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
	WatchWorkspaceRequestSchema,
	SyncRequestSchema,
	SyncResponseSchema,
	WorkspaceService,
	WorkspaceState,
	type Base,
	type Command,
	type CreateWorkspaceRequest,
	type DaemonLogEntry,
	type DaemonLogSource,
	type FinishedCommand,
	type Node,
	type PlacementDecision,
	type RunCommandRequest,
	type Session,
	type StopCommandRequest,
	type Workspace,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { WORKER_RPC_TIMEOUT_MS, type ControllerContext } from "../context.ts";
import { parseDurationMs } from "../durations.ts";
import { waitAtMost } from "../waits.ts";
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
// A sync's bundles pass through the controller to the worker, which applies
// them: big checkouts over slow links take minutes.
const SYNC_RPC_TIMEOUT_MS = 15 * 60_000;
// Waiting QueryOutput calls (--wait, --until) are sliced into bounded worker
// tunnel RPCs so a worker restart or silent tunnel drop never strands a call
// for the full 10-minute wait budget.
const QUERY_OUTPUT_SLICE_SEC = 30;
const QUERY_OUTPUT_RECONNECT_GRACE_MS = 30_000;
const QUERY_OUTPUT_RETRY_POLL_MS = 250;
const WORKER_PROBE_TIMEOUT_MS = 5_000;
const DAEMON_LOG_WORKER_TIMEOUT_MS = 5_000;
const DEFAULT_DAEMON_LOG_LIMIT = 200;
const MAX_DAEMON_LOG_LIMIT = 1000;
const UPGRADE_WAIT_MS = 90_000;
const CAPACITY_WAIT_MS = 35_000;
const WORKSPACE_RACE_HEDGE_MS = 2_000;
const WORKSPACE_RACE_PREPARE_WAIT_MS = 1_500;

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
		// An ended command's memory is its peak, as its worker reports it
		// the moment it ends.
		memoryBytes: f.peakMemoryBytes,
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
		samples: f.samples,
		estimate: f.estimate ? clone(CommandEstimateSchema, f.estimate) : undefined,
		placement: f.placement ? clone(PlacementDecisionSchema, f.placement) : undefined,
	});
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
		const baseRuntime =
			b.runtime || (b.cladefsCell ? (b.id.includes("-vm-") ? "vm" : "cladefs") : "");
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
				await waitAtMost(waiter.promise, rem, signal);
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
				await waitAtMost(Promise.race(waiters.map((w) => w.promise)), rem, signal);
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

	async function waitCapacity(
		hosts: string[],
		deadlineMs: number,
		signal?: AbortSignal,
	): Promise<void> {
		if (signal?.aborted) return;
		const rem = deadlineMs - Date.now();
		if (rem <= 0) return;
		const waiters = [
			ctrl.capacityChanges.wait("*"),
			...hosts.map((h) => ctrl.power.changes.wait(h)),
		];
		try {
			await waitAtMost(Promise.race(waiters.map((w) => w.promise)), rem, signal);
		} finally {
			for (const w of waiters) w.cancel();
		}
	}

	function formatWaitSuffix(waitStartMs: number): string {
		if (waitStartMs <= 0) return "";
		const secs = Math.max(1, Math.round((Date.now() - waitStartMs) / 1000));
		return ` (waited ${secs}s for capacity)`;
	}

	function finalizeDecision(
		d: PlacementDecision,
		winner: Candidate,
		reason: string,
	): PlacementDecision {
		d.nodeId = winner.view.node.id;
		d.hostname = winner.host;
		d.reason = reason;
		for (const pc of d.candidates) {
			pc.chosen = pc.nodeId === d.nodeId;
		}
		ctrl.store.addPlacement(d, ctrl.now());
		return d;
	}

	function allocateWorkspaceLocked(
		sess: Session,
		projectName: string,
		hosted: ReturnType<ControllerContext["hosted"]>,
		node: Candidate,
		req: CreateWorkspaceRequest,
		syncHost: string,
	): CreateWorkspaceRequest {
		const chosenArch = node.view.hb?.resources?.arch ?? "";
		const reqRuntime = req.placement?.bench ? "cladefs" : (req.placement?.runtime ?? "");
		const chosenRuntimes = (node.view.hb?.resources?.runtimes ?? []).filter(
			(r) => r === "vm" || r === "cladefs",
		);
		const sharedBaseRuntime = reqRuntime || chosenRuntimes[0] || "";
		const sharedBase = ctrl.store.latestSharedBase(
			projectName,
			chosenArch,
			ctrl.cladefsTenantId,
			sharedBaseRuntime,
		);
		const fencingToken = ctrl.store.nextFencingToken();
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
			throwaway: req.throwaway,
		});
		const ok = ctrl.store.addLiveWorkspace(
			fwd.id,
			sess.id,
			node.view.node.id,
			sess.agent,
			ctrl.now(),
			req.throwaway,
		);
		if (!ok) {
			const cur = ctrl.store.getSession(sess.id);
			throw sessionEndedError(sess.id, cur.endReason);
		}
		ctrl.markCreatingWS(fwd.id);
		return fwd;
	}

	function discardRacedWorkspace(
		node: Candidate,
		fwd: CreateWorkspaceRequest,
		ac: AbortController,
		promise: Promise<{ ws?: Workspace; err?: unknown }>,
	): void {
		ac.abort();
		void promise.then(async (res) => {
			if (res.ws) {
				await releaseFailedWorkspace(res.ws.id).catch(() => {});
				return;
			}
			// Best-effort cleanup in case the worker already created the workspace
			// right as the controller aborted the CreateWorkspace RPC.
			try {
				await ctrl.tunnel.callUnary(
					node.view.node.id,
					node.host,
					"/hotbed.v1.WorkspaceService/ReleaseWorkspace",
					ReleaseWorkspaceRequestSchema,
					create(ReleaseWorkspaceRequestSchema, { id: fwd.id }),
					ReleaseWorkspaceResponseSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
			} catch {
				// Worker never created it or is unreachable.
			}
		});
	}

	function linkAbort(parent?: AbortSignal): { ac: AbortController; unlink: () => void } {
		const ac = new AbortController();
		if (!parent) return { ac, unlink: () => {} };
		if (parent.aborted) {
			ac.abort();
			return { ac, unlink: () => {} };
		}
		const onAbort = () => ac.abort();
		parent.addEventListener("abort", onAbort, { once: true });
		return { ac, unlink: () => parent.removeEventListener("abort", onAbort) };
	}

	async function raceWorkspaces(
		sess: Session,
		req: CreateWorkspaceRequest,
		syncHost: string,
		primary: Candidate,
		primaryFwd: CreateWorkspaceRequest,
		runnerUp: Candidate,
		runnerUpFwdInitial: CreateWorkspaceRequest | undefined,
		decision: PlacementDecision,
		waitStartMs: number,
		signal?: AbortSignal,
	): Promise<{ ws?: Workspace; skipIds?: string[]; err?: unknown }> {
		let releaseSessionRace!: () => void;
		let raceToken: Promise<void> | null = null;
		if (!req.throwaway) {
			raceToken = new Promise<void>((r) => {
				releaseSessionRace = r;
			});
			ctrl.sessionRaces.set(sess.id, raceToken);
		}
		const l1 = linkAbort(signal);
		const l2 = linkAbort(signal);
		type Attempt = {
			node: Candidate;
			fwd: CreateWorkspaceRequest;
			ac: AbortController;
			promise: Promise<{ ws?: Workspace; err?: unknown }>;
			done: boolean;
			res?: { ws?: Workspace; err?: unknown };
		};
		const startAttempt = (
			node: Candidate,
			fwd: CreateWorkspaceRequest,
			ac: AbortController,
		): Attempt => {
			const att: Attempt = {
				node,
				fwd,
				ac,
				done: false,
				promise: makeWorkspace(sess, node, fwd, ac.signal).then(
					(ws) => {
						att.done = true;
						att.res = { ws };
						return att.res;
					},
					(err) => {
						att.done = true;
						att.res = { err };
						return att.res;
					},
				),
			};
			return att;
		};

		const waitSuffix = formatWaitSuffix(waitStartMs);
		const baseReason = decision.reason;

		try {
			const a1 = startAttempt(primary, primaryFwd, l1.ac);
			let a2: Attempt | null = runnerUpFwdInitial
				? startAttempt(runnerUp, runnerUpFwdInitial, l2.ac)
				: null;

			const launchRunnerUp = async (): Promise<Attempt | null> => {
				if (a2 || signal?.aborted) return a2;
				try {
					const fwd2 = await ctrl.withPlacementLock(() => {
						const p = ctrl.store.getProject(sess.project);
						const hosted = ctrl.hosted(p);
						if (!req.throwaway) {
							const existing = ctrl.store
								.sessionWorkspaces(sess.id)
								.find((w) => !w.throwaway && w.node === runnerUp.view.node.id);
							if (existing) return null;
						}
						ctrl.power.wake(runnerUp.host, ctrl.now());
						return allocateWorkspaceLocked(sess, p.name, hosted, runnerUp, req, syncHost);
					});
					if (!fwd2) return null;
					a2 = startAttempt(runnerUp, fwd2, l2.ac);
					return a2;
				} catch {
					return null;
				}
			};

			// Hedged mode: give the clearly cheaper primary a short head start. If it
			// returns a ready workspace before the hedge timer, never touch runnerUp.
			if (!a2) {
				let timer: ReturnType<typeof setTimeout> | undefined;
				const hedge = new Promise<"hedge">((resolve) => {
					timer = setTimeout(() => resolve("hedge"), WORKSPACE_RACE_HEDGE_MS);
				});
				const first = await Promise.race([a1.promise.then(() => "p1" as const), hedge]);
				if (timer !== undefined) clearTimeout(timer);
				if (first === "p1") {
					const r1 = a1.res!;
					if (r1.ws && (r1.ws.state !== WorkspaceState.PREPARING || !runnerUp.warm)) {
						r1.ws.placement = finalizeDecision(decision, primary, baseReason + waitSuffix);
						return { ws: r1.ws };
					}
					if (r1.err && (signal?.aborted || !passOver(r1.err))) {
						return { err: r1.err };
					}
					// Primary returned PREPARING while runnerUp has a warm base, or primary
					// failed with passOver: immediately launch runnerUp to race or take over.
					await launchRunnerUp();
				} else {
					// Primary is still creating past the hedge delay: start runnerUp in parallel.
					await launchRunnerUp();
				}
			}

			if (!a2) {
				const r1 = await a1.promise;
				if (r1.ws) {
					r1.ws.placement = finalizeDecision(decision, primary, baseReason + waitSuffix);
					return { ws: r1.ws };
				}
				return {
					err: r1.err,
					skipIds: passOver(r1.err) ? [primary.view.node.id] : undefined,
				};
			}

			// Parallel race between primary (a1) and runnerUp (a2).
			// First READY (!PREPARING) workspace wins immediately; PREPARING waits for
			// the other racer in case the other finishes READY.
			const isReady = (r?: { ws?: Workspace; err?: unknown }) =>
				Boolean(r?.ws && r.ws.state !== WorkspaceState.PREPARING);

			if (!a1.done && !a2.done) {
				await Promise.race([a1.promise, a2.promise]);
			}

			if (isReady(a1.res)) {
				discardRacedWorkspace(a2.node, a2.fwd, a2.ac, a2.promise);
				a1.res!.ws!.placement = finalizeDecision(
					decision,
					primary,
					`${baseReason} (raced ${runnerUp.host})${waitSuffix}`,
				);
				return { ws: a1.res!.ws };
			}
			if (isReady(a2.res)) {
				discardRacedWorkspace(a1.node, a1.fwd, a1.ac, a1.promise);
				a2.res!.ws!.placement = finalizeDecision(
					decision,
					runnerUp,
					`won workspace race against ${primary.host}${waitSuffix}`,
				);
				return { ws: a2.res!.ws };
			}

			// Neither is READY yet. If one already returned PREPARING while the other
			// is still in flight, give the other racer a short grace window to finish
			// READY before picking the PREPARING workspace and discarding the slow one.
			if ((a1.done && a1.res?.ws) || (a2.done && a2.res?.ws)) {
				const pending = a1.done ? a2.promise : a1.promise;
				await waitAtMost(
					pending.then(() => {}),
					WORKSPACE_RACE_PREPARE_WAIT_MS,
					signal,
				);
			} else {
				await Promise.all([a1.promise, a2.promise]);
			}
			const r1 = a1.res;
			const r2 = a2.res;
			if (isReady(r1)) {
				discardRacedWorkspace(a2.node, a2.fwd, a2.ac, a2.promise);
				r1!.ws!.placement = finalizeDecision(
					decision,
					primary,
					`${baseReason} (raced ${runnerUp.host})${waitSuffix}`,
				);
				return { ws: r1!.ws };
			}
			if (isReady(r2)) {
				discardRacedWorkspace(a1.node, a1.fwd, a1.ac, a1.promise);
				r2!.ws!.placement = finalizeDecision(
					decision,
					runnerUp,
					`won workspace race against ${primary.host}${waitSuffix}`,
				);
				return { ws: r2!.ws };
			}
			// Both are PREPARING, or one succeeded as PREPARING while the other failed/stalled.
			if (r1?.ws) {
				discardRacedWorkspace(a2.node, a2.fwd, a2.ac, a2.promise);
				r1.ws.placement = finalizeDecision(
					decision,
					primary,
					`${baseReason} (raced ${runnerUp.host})${waitSuffix}`,
				);
				return { ws: r1.ws };
			}
			if (r2?.ws) {
				discardRacedWorkspace(a1.node, a1.fwd, a1.ac, a1.promise);
				r2.ws.placement = finalizeDecision(
					decision,
					runnerUp,
					`won workspace race against ${primary.host}${waitSuffix}`,
				);
				return { ws: r2.ws };
			}
			// Both failed.
			const skipIds: string[] = [];
			if (r1 && passOver(r1.err)) skipIds.push(primary.view.node.id);
			if (r2 && passOver(r2.err)) skipIds.push(runnerUp.view.node.id);
			return { err: r1?.err ?? r2?.err, skipIds: skipIds.length > 0 ? skipIds : undefined };
		} finally {
			l1.unlink();
			l2.unlink();
			if (raceToken) {
				if (ctrl.sessionRaces.get(sess.id) === raceToken) {
					ctrl.sessionRaces.delete(sess.id);
				}
				releaseSessionRace();
			}
		}
	}

	async function createWorkspaceInternal(
		req: CreateWorkspaceRequest,
		syncHost: string,
		signal?: AbortSignal,
	): Promise<Workspace> {
		const requestedRt = req.placement?.runtime ?? req.runtime ?? "";
		if (requestedRt && requestedRt !== "vm" && requestedRt !== "cladefs") {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`unknown runtime "${requestedRt}" (want vm or cladefs)`,
			).withSuggestion("use --runtime vm or --runtime cladefs");
		}
		const sess = ctrl.store.liveSession(req.sessionId, ctrl.now());
		const skip = new Set<string>();
		let lastErr: unknown = null;
		let upgradeDeadline = 0;
		let capacityWaitStartedAt = 0;
		let capacityDeadline = 0;

		while (true) {
			if (!req.throwaway) {
				const inFlightRace = ctrl.sessionRaces.get(sess.id);
				if (inFlightRace) {
					await inFlightRace;
					if (signal?.aborted) throw hotbedError(ErrorCode.Internal, "placement canceled");
				}
			}
			const claimed = await claim(sess, req, syncHost, skip, signal, capacityWaitStartedAt);
			if (claimed.allBusy) {
				const hosts = claimed.busyHosts ?? [];
				if (capacityWaitStartedAt === 0) {
					capacityWaitStartedAt = Date.now();
					capacityDeadline =
						capacityWaitStartedAt +
						Math.min(CAPACITY_WAIT_MS, ctrl.power.wakeWaitMs || CAPACITY_WAIT_MS);
				}
				if (signal?.aborted) {
					throw hotbedError(ErrorCode.Internal, "placement canceled");
				}
				if (Date.now() >= capacityDeadline) {
					throw hotbedError(
						ErrorCode.NodesBusy,
						`all eligible nodes are currently full (${hosts.join(", ")}); queued at controller, waiting for any to free up`,
						{
							retryable: true,
							suggestion: "hotbed run waits automatically; pass --node HOST to queue on one node",
						},
					);
				}
				await waitCapacity(hosts, capacityDeadline, signal);
				continue;
			}
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
					ws.placement = claimed.decision;
					return ws;
				} catch (err) {
					if (codeOf(err) === ErrorCode.NotFound && !ctrl.creatingWS.has(node.mine)) {
						ctrl.store.reconcileMissingWorkspace(node.mine, ctrl.now());
						continue;
					}
					throw err;
				}
			}

			if (claimed.runnerUp && claimed.decision) {
				const raced = await raceWorkspaces(
					sess,
					req,
					syncHost,
					node,
					claimed.fwd,
					claimed.runnerUp,
					claimed.runnerUpFwd,
					claimed.decision,
					capacityWaitStartedAt,
					signal,
				);
				if (raced.ws) return raced.ws;
				if (signal?.aborted || !raced.skipIds || raced.skipIds.length === 0) {
					throw raced.err;
				}
				for (const id of raced.skipIds) skip.add(id);
				lastErr = raced.err;
				continue;
			}

			try {
				const ws = await makeWorkspace(sess, node, claimed.fwd, signal);
				ws.placement = claimed.decision;
				return ws;
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
		waitStartMs = 0,
	): Promise<{
		node?: Candidate;
		fwd?: CreateWorkspaceRequest;
		runnerUp?: Candidate;
		runnerUpFwd?: CreateWorkspaceRequest;
		decision?: PlacementDecision;
		allBusy?: boolean;
		busyHosts?: string[];
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
					// A throwaway workspace is no one's to run in again, and a
					// throwaway run gets a new workspace wherever it goes.
					if (!w.throwaway && !req.throwaway) mine.set(w.node, w.id);
				}
				if (needRelease.length > 0 || needProbe.length > 0) {
					return { kind: "reconcile" as const, needRelease, needProbe };
				}
				const filtered = nvs.filter((nv) => !skip.has(nv.node.id));
				const reqRuntimeForWarm = req.placement?.bench ? "cladefs" : (req.placement?.runtime ?? "");
				const cs = buildCandidates(
					filtered,
					p.name,
					mine,
					(res) => ctrl.follows(res),
					(arch, runtimes) => {
						const targetRuntimes = reqRuntimeForWarm
							? runtimes.includes(reqRuntimeForWarm)
								? [reqRuntimeForWarm]
								: []
							: runtimes;
						return targetRuntimes.some((rt) =>
							ctrl.store.hasSharedBase(p.name, arch, ctrl.cladefsTenantId, rt),
						);
					},
				);
				const { chosen, runnerUp, raceImmediate, allBusy, busyHosts, why, err } = chooseCandidate(
					cs,
					req.placement,
					p.hosts,
					req.syncDir !== "",
					{ waitForCapacity: req.waitForCapacity },
				);
				if (allBusy) {
					return { kind: "done" as const, allBusy: true, busyHosts, upgrading: [] };
				}
				const waitSuffix = formatWaitSuffix(waitStartMs);
				const d = buildPlacementDecision(
					cs,
					req.placement,
					p.hosts,
					chosen,
					runnerUp ? why : why + waitSuffix,
					err,
					{
						noSync: req.syncDir === "",
						throwaway: req.throwaway,
					},
				);
				d.sessionId = sess.id;
				d.project = p.name;
				d.repo = req.repo;
				if (!runnerUp) {
					ctrl.store.addPlacement(d, ctrl.now());
				}
				if (err || !chosen) {
					const up = upgradingFor(cs, req.placement, p.hosts, req.syncDir !== "");
					return { kind: "done" as const, decision: d, upgrading: up, err };
				}
				if (chosen.asleep) {
					return { kind: "done" as const, node: chosen, decision: d, upgrading: [] };
				}
				ctrl.power.wake(chosen.host, ctrl.now());
				if (chosen.mine) {
					return { kind: "done" as const, node: chosen, decision: d, upgrading: [] };
				}
				const fwd = allocateWorkspaceLocked(sess, p.name, hosted, chosen, req, syncHost);
				let runnerUpFwd: CreateWorkspaceRequest | undefined;
				if (runnerUp && raceImmediate) {
					ctrl.power.wake(runnerUp.host, ctrl.now());
					runnerUpFwd = allocateWorkspaceLocked(sess, p.name, hosted, runnerUp, req, syncHost);
				}
				return {
					kind: "done" as const,
					node: chosen,
					fwd,
					runnerUp,
					runnerUpFwd,
					decision: d,
					upgrading: [],
				};
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
				if (signal?.aborted) {
					// The worker may still finish creating the workspace after we aborted
					// the RPC; mark it released instead of dropping the row so toRelease()
					// reaps it on the worker's next heartbeat.
					ctrl.store.releaseWorkspaceRow(fwd.id, ctrl.now());
				} else {
					ctrl.store.dropWorkspace(fwd.id);
				}
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
				const c = codeOf(err);
				if (signal?.aborted) throw err;
				if (c === ErrorCode.NodesBusy) continue;
				if (c !== ErrorCode.NodeWaking || Date.now() > deadline) {
					throw err;
				}
			}
		}
	}

	async function runCommandInternal(req: RunCommandRequest): Promise<Command> {
		const { host, row, project } = workspaceOf(req.workspaceId);
		const fwd = clone(RunCommandRequestSchema, req);
		if (!fwd.id) fwd.id = newId();
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

		async *watchWorkspace(req, ctx) {
			const { host, row } = workspaceOf(req.id);
			try {
				yield* ctrl.tunnel.callServerStream(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/WatchWorkspace",
					WatchWorkspaceRequestSchema,
					req,
					WorkspaceSchema,
					ctx.signal,
					(headers) => {
						const progress = headers["Hotbed-Progress"];
						if (progress) ctx.responseHeader.set("Hotbed-Progress", progress);
					},
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
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

		async sync(reqs, ctx) {
			// The first message names the workspace; the rest are its
			// bundles' bytes, passed on as they come.
			const it = reqs[Symbol.asyncIterator]();
			const first = await it.next();
			if (first.done || first.value.msg.case !== "header") {
				throw hotbedError(ErrorCode.InvalidArgs, "a sync starts with its header");
			}
			const head = first.value;
			const header = first.value.msg.value;
			const { host, row } = workspaceOf(header.workspaceId);
			async function* all() {
				yield head;
				for (let r = await it.next(); !r.done; r = await it.next()) yield r.value;
			}
			try {
				return await ctrl.tunnel.callClientStream(
					row.node,
					host,
					"/hotbed.v1.WorkspaceService/Sync",
					SyncRequestSchema,
					all(),
					SyncResponseSchema,
					SYNC_RPC_TIMEOUT_MS,
					ctx.signal,
				);
			} catch (err) {
				throw fromWorkerError(host, err);
			}
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

		async runCommand(req) {
			return runCommandInternal(req);
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

		async stopCommand(req, ctx) {
			const fwd = clone(StopCommandRequestSchema, req);
			fwd.host = getCaller(ctx.values).hostname;
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

		async pauseCommand(req, ctx) {
			const fwd = clone(PauseCommandRequestSchema, req);
			fwd.host = getCaller(ctx.values).hostname;
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

		async resumeCommand(req, ctx) {
			const fwd = clone(ResumeCommandRequestSchema, req);
			fwd.host = getCaller(ctx.values).hostname;
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

		async *followLog(req, ctx) {
			// The command's worker has its output as its log keeps it (cut
			// past its limit); what the controller keeps of it serves a
			// reader the worker cannot answer, gone or forgetful.
			const reqOffset = Number(req.offset);
			let relayed = false;
			let failed: unknown = null;
			let host = "";
			try {
				const route = commandOf(req.id);
				host = route.host;
				for await (const chunk of ctrl.tunnel.callServerStream(
					route.nodeId,
					route.host,
					"/hotbed.v1.WorkspaceService/FollowLog",
					FollowLogRequestSchema,
					req,
					LogChunkSchema,
					ctx.signal,
				)) {
					relayed = true;
					yield chunk;
				}
				return;
			} catch (err) {
				failed = host ? fromWorkerError(host, err) : err;
				const code = codeOf(failed);
				if (
					relayed ||
					req.follow ||
					req.wait ||
					(code !== ErrorCode.WorkerUnreachable && code !== ErrorCode.NotFound)
				) {
					throw failed;
				}
			}
			const stored = await ctrl.commands.readOutputChunks(req.id, reqOffset);
			if (!stored.hasOutput) throw failed;
			for (const c of stored.chunks) {
				let data = c.data;
				// If caller requested a mid-chunk offset on normal (non-marker) data, slice to reqOffset.
				if (reqOffset > c.offset && c.nextOffset === c.offset + c.data.byteLength) {
					const skip = reqOffset - c.offset;
					if (skip >= data.byteLength) continue;
					data = data.subarray(skip);
				}
				yield create(LogChunkSchema, {
					data,
					nextOffset: BigInt(c.nextOffset),
					receivedAt: req.timestamps ? BigInt(c.receivedAt) : 0n,
				});
			}
		},

		async getLogLines(req) {
			const effectiveTail = req.tail > 0 ? req.tail : 2000;
			const streamed = await ctrl.commands.getOutputLines(req.id, effectiveTail);
			if (streamed.hasOutput) {
				return create(CommandLogSchema, {
					commandId: req.id,
					lines: streamed.lines,
					cut: streamed.cut,
					lineTimestamps: streamed.lineTimestamps.map((t) => BigInt(t)),
				});
			}
			const finished = ctrl.store.finishedCommand(req.id);
			let cached = finished ? await ctrl.commands.getOutputLog(req.id) : null;
			if (cached && (!cached.cut || cached.tail >= effectiveTail)) {
				return sliceCommandLog(cached.log, effectiveTail);
			}
			try {
				const { host, nodeId } = commandOf(req.id);
				const res = await ctrl.tunnel.callUnary(
					nodeId,
					host,
					"/hotbed.v1.WorkspaceService/GetLogLines",
					GetLogLinesRequestSchema,
					req,
					CommandLogSchema,
					WORKER_RPC_TIMEOUT_MS,
				);
				if (finished || ctrl.store.finishedCommand(req.id)) {
					await ctrl.commands.saveOutputLog(req.id, effectiveTail, res, ctrl.now());
				}
				return res;
			} catch (err) {
				if (!cached) {
					cached = await ctrl.commands.getOutputLog(req.id);
				}
				if (cached) {
					return sliceCommandLog(cached.log, effectiveTail);
				}
				let host = "";
				try {
					const route = ctrl.store.lookupCommand(req.id);
					host = ctrl.store.getNode(route.node).node.hostname;
				} catch {
					// Command or node not found; rethrow original error below.
				}
				if (host) {
					throw fromWorkerError(host, err);
				}
				throw err;
			}
		},

		async queryOutput(req, ctx) {
			const route = ctrl.store.lookupCommand(req.id);
			if (route.session) ctrl.store.touchSession(route.session, ctrl.now());
			const { host } = ensureOnlineWorker(route.node);
			const nodeId = route.node;
			const backfillFinished = (out: MessageShape<typeof OutputQuerySchema>) => {
				if (out.state === CommandState.UNSPECIFIED) {
					// Its worker has forgotten it, ended: history has how.
					const f = ctrl.store.finishedCommand(req.id);
					if (f) {
						out.state = f.state;
						out.exitCode = f.exitCode;
					}
				}
				return out;
			};

			if ((!req.wait && !req.until) || req.timeoutSeconds < 0) {
				try {
					const out = await ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/QueryOutput",
						QueryOutputRequestSchema,
						req,
						OutputQuerySchema,
						WORKER_RPC_TIMEOUT_MS,
						ctx.signal,
					);
					return backfillFinished(out);
				} catch (err) {
					throw fromWorkerError(host, err);
				}
			}

			// Slice waiting queries (--wait, --until) into bounded worker tunnel
			// calls and ride out brief worker reconnects (e.g. auto-upgrade) within
			// the caller's timeout budget so a dead socket never strands a wait.
			const totalWaitMs = Math.min(Math.max(req.timeoutSeconds || 60, 0), 600) * 1000;
			const deadline = Date.now() + totalWaitMs;
			let unreachableSince = 0;

			while (true) {
				const remainingMs = Math.max(0, deadline - Date.now());
				const sliceSec = Math.max(
					1,
					Math.min(QUERY_OUTPUT_SLICE_SEC, Math.ceil(remainingMs / 1000)),
				);
				const sliceReq = clone(QueryOutputRequestSchema, req);
				sliceReq.timeoutSeconds = sliceSec;
				try {
					const out = await ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/QueryOutput",
						QueryOutputRequestSchema,
						sliceReq,
						OutputQuerySchema,
						WORKER_RPC_TIMEOUT_MS + sliceSec * 1000,
						ctx.signal,
					);
					unreachableSince = 0;
					backfillFinished(out);
					if (out.waited !== "timeout" || Date.now() >= deadline || ctx.signal.aborted) {
						return out;
					}
					if (route.session) ctrl.store.touchSession(route.session, ctrl.now());
				} catch (err) {
					const werr = fromWorkerError(host, err);
					if (werr.code === ErrorCode.WorkerUnreachable && werr.retryable && !ctx.signal.aborted) {
						const now = Date.now();
						if (unreachableSince === 0) unreachableSince = now;
						const retryDeadline = Math.min(
							deadline,
							unreachableSince + QUERY_OUTPUT_RECONNECT_GRACE_MS,
						);
						const left = retryDeadline - now;
						if (left > 0) {
							await sleepAbortable(Math.min(QUERY_OUTPUT_RETRY_POLL_MS, left), ctx.signal);
							if (!ctx.signal.aborted) {
								continue;
							}
						}
					}
					throw werr;
				}
			}
		},

		async detachCommand(req) {
			return byHand(
				req.id,
				req.sessionId,
				false,
				"/hotbed.v1.WorkspaceService/DetachCommand",
				(nodeId, host, t) =>
					ctrl.tunnel.callUnary(
						nodeId,
						host,
						"/hotbed.v1.WorkspaceService/DetachCommand",
						DetachCommandRequestSchema,
						req,
						CommandSchema,
						t,
					),
			);
		},

		async sendInput(req) {
			const route = ctrl.store.lookupCommand(req.id);
			if (req.sessionId && req.sessionId !== route.session) {
				throw hotbedError(ErrorCode.NotFound, `session ${req.sessionId} has no command ${req.id}`);
			}
			const { host } = ensureOnlineWorker(route.node);
			try {
				return await ctrl.tunnel.callUnary(
					route.node,
					host,
					"/hotbed.v1.WorkspaceService/SendInput",
					SendInputRequestSchema,
					req,
					SendInputResponseSchema,
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
				const stored = await ctrl.commands.getProfile(id);
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

function sliceCommandLog(
	log: import("../gen/hotbed/v1/hotbed_pb.ts").CommandLog,
	tail: number,
): import("../gen/hotbed/v1/hotbed_pb.ts").CommandLog {
	if (tail <= 0 || log.lines.length <= tail) return log;
	const start = log.lines.length - tail;
	const lineTimestamps =
		log.lineTimestamps.length === log.lines.length ? log.lineTimestamps.slice(start) : [];
	return create(CommandLogSchema, {
		commandId: log.commandId,
		lines: log.lines.slice(start),
		cut: log.cut,
		lineTimestamps,
	});
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

function sleepAbortable(ms: number, signal?: AbortSignal): Promise<void> {
	if (ms <= 0 || signal?.aborted) return Promise.resolve();
	return new Promise<void>((resolve) => {
		const done = () => {
			clearTimeout(timer);
			signal?.removeEventListener("abort", done);
			resolve();
		};
		const timer = setTimeout(done, ms);
		signal?.addEventListener("abort", done, { once: true });
	});
}
