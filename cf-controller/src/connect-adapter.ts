import {
	createContextKey,
	createContextValues,
	createConnectRouter,
	type ConnectRouter,
	type ContextValues,
	type Interceptor,
} from "@connectrpc/connect";
import {
	universalServerRequestFromFetch,
	universalServerResponseToFetch,
	type UniversalHandler,
} from "@connectrpc/connect/protocol";
import { getOption, hasOption, isFieldSet, type Message } from "@bufbuild/protobuf";
import { param } from "./gen/hotbed/v1/hotbed_pb.ts";
import type { CallerIdentity } from "./auth.ts";
import { ErrorCode, hotbedError, hotbedErrorInterceptor } from "./errors.ts";

export const CALLER_KEY = createContextKey<CallerIdentity | undefined>(undefined, {
	description: "Authenticated hotbed caller identity",
});

export function getCaller(ctx: ContextValues): CallerIdentity {
	const c = ctx.get(CALLER_KEY);
	if (!c) {
		throw hotbedError(ErrorCode.NotAuthorized, "this server admits no one: its gate is missing");
	}
	return c;
}

/**
 * Refuses any request message that sets a field annotated with `(param) = { local: true }`.
 * Mirrors Go's internal/httpx.noLocalFields interceptor.
 */
const noLocalFieldsInterceptor: Interceptor = (next) => async (req) => {
	if (!req.stream) {
		const msg = req.message as Message;
		const desc = req.method.input;
		for (const field of desc.fields) {
			if (hasOption(field, param) && getOption(field, param).local && isFieldSet(msg, field)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`${desc.typeName}.${field.name} is the client's alone (local): no request to the server sets it`,
				).withSuggestion("leave it unset; only hotbed's commands read it");
			}
		}
	}
	return next(req);
};

const WORKER_ALLOWED_PROCEDURES = new Set([
	"/hotbed.v1.ClusterService/Join",
	"/hotbed.v1.ClusterService/Heartbeat",
	"/hotbed.v1.ClusterService/PushEvents",
	"/hotbed.v1.ClusterService/GetBuild",
	"/hotbed.v1.ClusterService/SaveCommandDiagnostics",
	"/hotbed.v1.ClusterService/SaveCommandSample",
	"/hotbed.v1.ClusterService/PushCommandOutput",
	"/hotbed.v1.ClusterService/Standby",
	"/hotbed.v1.ClusterService/ReportHostUsage",
	// Its own config only (getWorkerConfig refuses any other scope): a worker
	// whose cached config enables no runtime asks for it before it can serve.
	"/hotbed.v1.ClusterService/GetWorkerConfig",
	"/hotbed.v1.GitAuthService/MintGitToken",
]);

export const VIEWER_ALLOWED_PROCEDURES = new Set([
	// ClusterService (read-only)
	"/hotbed.v1.ClusterService/ListNodes",
	"/hotbed.v1.ClusterService/GetBuild",
	"/hotbed.v1.ClusterService/ListRepos",
	"/hotbed.v1.ClusterService/ListSteps",
	"/hotbed.v1.ClusterService/ListProjects",
	"/hotbed.v1.ClusterService/GetProject",
	"/hotbed.v1.ClusterService/ListBases",
	"/hotbed.v1.ClusterService/ListFreshens",
	"/hotbed.v1.ClusterService/GetFreshen",
	"/hotbed.v1.ClusterService/GetSnapshot",
	"/hotbed.v1.ClusterService/GetCommandDiagnostics",
	"/hotbed.v1.ClusterService/ListCommandSamples",
	"/hotbed.v1.ClusterService/GetCommandSample",
	"/hotbed.v1.ClusterService/GetNode",
	"/hotbed.v1.ClusterService/ListPlacements",
	"/hotbed.v1.ClusterService/ListEvents",
	"/hotbed.v1.ClusterService/GetWorkerConfig",
	// SessionService (read-only)
	"/hotbed.v1.SessionService/GetSession",
	"/hotbed.v1.SessionService/ListSessions",
	"/hotbed.v1.SessionService/SearchSessions",
	"/hotbed.v1.SessionService/WatchSession",
	"/hotbed.v1.SessionService/Poll",
	// WorkspaceService (read-only)
	"/hotbed.v1.WorkspaceService/GetWorkspace",
	"/hotbed.v1.WorkspaceService/WatchWorkspace",
	"/hotbed.v1.WorkspaceService/ListWorkspaces",
	"/hotbed.v1.WorkspaceService/ListFiles",
	"/hotbed.v1.WorkspaceService/GetFile",
	"/hotbed.v1.WorkspaceService/GetCommand",
	"/hotbed.v1.WorkspaceService/ListCommands",
	"/hotbed.v1.WorkspaceService/FollowLog",
	"/hotbed.v1.WorkspaceService/GetLogLines",
	"/hotbed.v1.WorkspaceService/QueryOutput",
	"/hotbed.v1.WorkspaceService/InspectCommand",
	"/hotbed.v1.WorkspaceService/GetProfile",
	"/hotbed.v1.WorkspaceService/GetDaemonLogs",
	// ScheduleService (read-only)
	"/hotbed.v1.ScheduleService/ListSchedules",
	"/hotbed.v1.ScheduleService/GetSchedule",
	// QuestionService (read-only)
	"/hotbed.v1.QuestionService/ListQuestions",
	"/hotbed.v1.QuestionService/ListQuestionProjects",
	"/hotbed.v1.QuestionService/GetQuestion",
	// ReviewService (read-only)
	"/hotbed.v1.ReviewService/ListReviews",
	"/hotbed.v1.ReviewService/GetReview",
	"/hotbed.v1.ReviewService/ListResponses",
	// GitAuthService (read-only metadata)
	"/hotbed.v1.GitAuthService/ListGitCredentials",
	// MemoryService (read-only)
	"/hotbed.v1.MemoryService/ListMemories",
	"/hotbed.v1.MemoryService/GetMemories",
	"/hotbed.v1.MemoryService/ShowMemory",
	// BoardService (read-only)
	"/hotbed.v1.BoardService/ListBoardThreads",
	"/hotbed.v1.BoardService/GetBoardThread",
	"/hotbed.v1.BoardService/ListBoardEvents",
	"/hotbed.v1.BoardService/ListBoardActors",
	"/hotbed.v1.BoardService/DownloadBoardAttachment",
	"/hotbed.v1.BoardService/ListBoardSchemas",
	"/hotbed.v1.BoardService/GetBoardSchema",
	"/hotbed.v1.BoardService/ListBoardSchedules",
	"/hotbed.v1.BoardService/ListBoardTasks",
	// IssueService (read-only)
	"/hotbed.v1.IssueService/ListIssues",
	"/hotbed.v1.IssueService/GetIssue",
]);

const roleScopeInterceptor: Interceptor = (next) => async (req) => {
	const caller = getCaller(req.contextValues);
	const proc = `/${req.service.typeName}/${req.method.name}`;
	if (caller.role === "worker") {
		if (!WORKER_ALLOWED_PROCEDURES.has(proc)) {
			throw hotbedError(ErrorCode.NotAuthorized, `worker token is not authorized to call ${proc}`);
		}
	} else if (caller.orgRole === "viewer") {
		if (!VIEWER_ALLOWED_PROCEDURES.has(proc)) {
			throw hotbedError(
				ErrorCode.NotAuthorized,
				`viewer role is read-only and cannot call ${proc}`,
			).withSuggestion("ask an organization admin to upgrade your role to member or admin");
		}
	}
	return next(req);
};

export interface ConnectFetchDispatcher {
	hasPath(pathname: string): boolean;
	handle(req: Request, caller: CallerIdentity, buildNumber: number): Promise<Response>;
}

export function createConnectFetchDispatcher(
	register: (router: ConnectRouter) => void,
): ConnectFetchDispatcher {
	const router = createConnectRouter({
		connect: true,
		grpcWeb: true,
		grpc: true,
		interceptors: [hotbedErrorInterceptor, roleScopeInterceptor, noLocalFieldsInterceptor],
	});
	register(router);

	const byPath = new Map<string, UniversalHandler>();
	for (const h of router.handlers) {
		byPath.set(h.requestPath, h);
	}

	return {
		hasPath(pathname: string): boolean {
			return byPath.has(pathname);
		},
		async handle(req: Request, caller: CallerIdentity, buildNumber: number): Promise<Response> {
			const url = new URL(req.url);
			const handler = byPath.get(url.pathname);
			if (!handler) {
				return new Response("Not Found", { status: 404 });
			}
			const uReq = universalServerRequestFromFetch(req, { httpVersion: "2.0" });
			uReq.contextValues = createContextValues().set(CALLER_KEY, caller);
			const uRes = await handler(uReq);
			const res = universalServerResponseToFetch(uRes);
			const headers = new Headers(res.headers);
			headers.set("Hotbed-Build", String(buildNumber));
			return new Response(res.body, {
				status: res.status,
				statusText: res.statusText,
				headers,
			});
		},
	};
}
