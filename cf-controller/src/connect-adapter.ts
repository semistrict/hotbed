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
	"/hotbed.v1.ClusterService/Standby",
	"/hotbed.v1.ClusterService/ReportHostUsage",
	// Its own config only (getWorkerConfig refuses any other scope): a worker
	// whose cached config enables no runtime asks for it before it can serve.
	"/hotbed.v1.ClusterService/GetWorkerConfig",
	"/hotbed.v1.GitAuthService/MintGitToken",
]);

const workerRoleScopeInterceptor: Interceptor = (next) => async (req) => {
	const caller = getCaller(req.contextValues);
	if (caller.role === "worker") {
		const proc = `/${req.service.typeName}/${req.method.name}`;
		if (!WORKER_ALLOWED_PROCEDURES.has(proc)) {
			throw hotbedError(ErrorCode.NotAuthorized, `worker token is not authorized to call ${proc}`);
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
		interceptors: [hotbedErrorInterceptor, workerRoleScopeInterceptor, noLocalFieldsInterceptor],
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
