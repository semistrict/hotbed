// Which project a request means, and what of a project a node hosts: the
// project resolution every service shares, over ControllerContext.
import { create } from "@bufbuild/protobuf";
import type { CallerIdentity } from "./auth.ts";
import { PROJECT_NAME_RE, type ControllerContext, type ScopedRequest } from "./context.ts";
import { codeOf, ErrorCode, hotbedError } from "./errors.ts";
import {
	HostedProjectSchema,
	type HostedProject,
	type Project,
	type Repo,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { canonicalRemote, projectNameFor } from "./gitops.ts";

/** Projects enriched with live session, schedule, question, and review counts. */
export function projectsWithCounts(ctrl: ControllerContext): Project[] {
	const ps = ctrl.store.projects.list();
	const live = ctrl.store.sessions.liveByProject();
	const scheds = ctrl.store.schedules.countByProject();
	const qps = ctrl.store.questions.byProject();
	const qByName = new Map(qps.map((q) => [q.name, q]));
	const { active, toDecide } = ctrl.store.reviews.counts();
	const memCounts = ctrl.store.memories.pendingCounts();
	const boardCounts = ctrl.boardCountsFn?.();
	for (const p of ps) {
		p.liveSessions = live.get(p.name) ?? 0;
		p.schedules = scheds.get(p.name) ?? 0;
		p.activeReviews = active.get(p.name) ?? 0;
		p.reviewsToDecide = toDecide.get(p.name) ?? 0;
		p.pendingMemories = memCounts.byProject.get(p.name) ?? 0;
		const bc = boardCounts?.get(p.name);
		if (bc) {
			p.boardThreads = bc.threads;
			p.openBoardTasks = bc.openTasks;
			p.readyBoardTasks = bc.readyTasks;
			p.blockedBoardTasks = bc.blockedTasks;
		}
		const qp = qByName.get(p.name);
		if (qp) {
			p.openQuestions = qp.open;
			p.outbox = qp.outbox;
		}
	}
	return ps;
}

export function hostedOf(p: Project, repos: Repo[]): { hosted: HostedProject; missing: string[] } {
	const h = create(HostedProjectSchema, {
		name: p.name,
		steps: p.steps,
		scratchEpoch: p.scratchEpoch,
	});
	const missing: string[] = [];
	for (const remote of p.repos) {
		const match = repos.find((r) => canonicalRemote(r.url) === remote);
		if (!match) missing.push(remote);
		else h.repos.push(match);
	}
	return { hosted: h, missing };
}

export function hostedProject(ctrl: ControllerContext, p: Project): HostedProject {
	const { hosted, missing } = hostedOf(p, ctrl.store.projects.listRepos());
	if (missing.length > 0) {
		throw hotbedError(
			ErrorCode.NotFound,
			`project ${p.name} holds ${missing.join(", ")}, which no repo of the cluster has`,
		).withSuggestion(
			`hotbed repo add NAME ${missing[0]} -p ${p.name} registers it; or hotbed project rm-repo ${p.name} --repo ${missing[0]}`,
		);
	}
	if (hosted.repos.length === 0) {
		throw hotbedError(
			ErrorCode.NotFound,
			`project ${p.name} holds no repos to check out`,
		).withSuggestion(
			`hotbed project add ${p.name} --repo REMOTE, then hotbed repo add NAME REMOTE`,
		);
	}
	return hosted;
}

export function hostingFor(
	ctrl: ControllerContext,
	nodeId: string,
	hostname: string,
): { projects: HostedProject[]; release: string[] } {
	const projects = ctrl.store.projects.list();
	const repos = ctrl.store.projects.listRepos();
	const out: HostedProject[] = [];
	for (const p of projects) {
		if (p.hosts.length > 0 && !p.hosts.includes(hostname)) continue;
		const { hosted, missing } = hostedOf(p, repos);
		if (missing.length === 0 && hosted.repos.length > 0) out.push(hosted);
	}
	return { projects: out, release: ctrl.store.nodeState.toRelease(nodeId) };
}

export function repoById(ctrl: ControllerContext, id: string): Repo {
	const r = ctrl.store.projects.listRepos().find((x) => x.id === id);
	if (!r) {
		throw hotbedError(ErrorCode.NotFound, `no repo ${id}`).withSuggestion("hotbed repo ls");
	}
	return r;
}

export function listedProject(ctrl: ControllerContext, req: ScopedRequest): string {
	const sess = req.sessionId ? ctrl.store.sessions.get(req.sessionId) : null;
	if (req.scope.case === "allProjects" && req.scope.value) return "";
	if (req.scope.case === "project" && req.scope.value) {
		ctrl.store.projects.get(req.scope.value);
		return req.scope.value;
	}
	if (!sess && req.remote) {
		const remote = canonicalRemote(req.remote);
		const holding = ctrl.store.projects.holding(remote);
		if (holding.length === 1) return holding[0].name;
		if (holding.length === 0) {
			throw hotbedError(
				ErrorCode.NotFound,
				`say which project: no project holds ${remote}, this checkout's repo`,
			).withSuggestion("pass -p PROJECT or --all-projects (hotbed project ls)");
		}
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`say which project: ${remote}, this checkout's repo, is in projects ${holding.map((p) => p.name).join(", ")}`,
		).withSuggestion("pass -p PROJECT or --all-projects");
	}
	if (!sess) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"say which project: name one, all of them, or the session whose project it is",
		).withSuggestion("pass -p PROJECT, --all-projects, or -s SESSION");
	}
	if (!sess.project) {
		throw hotbedError(ErrorCode.InvalidArgs, `session ${sess.id} has no project`).withSuggestion(
			"pass -p PROJECT or --all-projects",
		);
	}
	return sess.project;
}

export function sessionProject(
	ctrl: ControllerContext,
	caller: CallerIdentity,
	name: string,
	remote: string,
): { project: Project; changed: boolean } {
	if (name) {
		return { project: ctrl.store.projects.get(name), changed: false };
	}
	if (!remote) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"a session works on a project: outside a git checkout, name it",
		).withSuggestion("pass -p PROJECT or set HOTBED_PROJECT (hotbed project ls)");
	}
	const holding = ctrl.store.projects.holding(remote);
	if (holding.length === 1) return { project: holding[0], changed: false };
	if (holding.length > 1) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`${remote} is in projects ${holding.map((p) => p.name).join(", ")}: name the session's project`,
		).withSuggestion("pass -p PROJECT or set HOTBED_PROJECT");
	}
	const own = projectNameFor(remote);
	if (!own) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`no project holds ${remote}, and its name makes none`,
		).withSuggestion(`make one: hotbed project add NAME --repo ${remote}`);
	}
	try {
		const existing = ctrl.store.projects.get(own);
		if (existing.repos.length > 0) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`no project holds ${remote}, and project ${own} holds other repos (${existing.repos.join(", ")})`,
			).withSuggestion(
				`add it there (hotbed project add ${own} --repo ${remote}), or pass -p PROJECT`,
			);
		}
	} catch (err) {
		if (codeOf(err) !== ErrorCode.NotFound) throw err;
	}
	const created = ctrl.store.projects.add(
		own,
		[remote],
		ctrl.madeBy(caller, `for ${remote}, which a session started in and no project held`),
	);
	return { project: created, changed: true };
}

export function ensureRepoProject(
	ctrl: ControllerContext,
	caller: CallerIdentity,
	repoId: string,
	repoUrl: string,
): Project | null {
	const remote = canonicalRemote(repoUrl);
	if (!remote) return null;
	const holding = ctrl.store.projects.holding(remote);
	if (holding.length === 1) return holding[0];
	if (holding.length > 1) return null;

	const candidates: string[] = [];
	const own = projectNameFor(remote);
	if (own) candidates.push(own);
	if (repoId && candidates[0] !== repoId && PROJECT_NAME_RE.test(repoId)) {
		candidates.push(repoId);
	}
	const base = candidates[0] ?? "repo";
	for (let n = 2; n <= 99; n++) {
		const suffix = `-${n}`;
		let prefix = base;
		if (prefix.length + suffix.length > 40) {
			prefix = prefix.slice(0, 40 - suffix.length).replace(/-+$/, "");
		}
		candidates.push(prefix + suffix);
	}
	for (const name of candidates) {
		try {
			const existing = ctrl.store.projects.get(name);
			if (existing.repos.length > 0) continue;
		} catch (err) {
			if (codeOf(err) !== ErrorCode.NotFound) throw err;
		}
		return ctrl.store.projects.add(
			name,
			[remote],
			ctrl.madeBy(caller, `for registered repo ${repoId}, which no project held`),
		);
	}
	throw hotbedError(
		ErrorCode.Conflict,
		`could not find a free project name for repo ${repoId}`,
	).withSuggestion("pass -p PROJECT to place the repo in an explicit project");
}
