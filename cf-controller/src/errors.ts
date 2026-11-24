import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import { ErrorInfoSchema } from "./gen/hotbed/v1/hotbed_pb.ts";

export const ErrorCode = {
	InvalidArgs: "INVALID_ARGS",
	AlreadyInitialized: "ALREADY_INITIALIZED",
	NotInitialized: "NOT_INITIALIZED",
	TailscaleUnavailable: "TAILSCALE_UNAVAILABLE",
	ControllerUnreachable: "CONTROLLER_UNREACHABLE",
	NotAuthorized: "NOT_AUTHORIZED",
	NotJoined: "NOT_JOINED",
	ClusterMismatch: "CLUSTER_MISMATCH",
	PlatformNotSupported: "PLATFORM_NOT_SUPPORTED",
	NotFound: "NOT_FOUND",
	Conflict: "CONFLICT",
	RepoUnreachable: "REPO_UNREACHABLE",
	WorkspaceFailed: "WORKSPACE_FAILED",
	StepFailed: "STEP_FAILED",
	StepNotIdempotent: "STEP_NOT_IDEMPOTENT",
	SyncRefused: "SYNC_REFUSED",
	WorkerUnreachable: "WORKER_UNREACHABLE",
	NodeWaking: "NODE_WAKING",
	CommandStopped: "COMMAND_STOPPED",
	Internal: "INTERNAL",
} as const;

export type HotbedErrorCode = (typeof ErrorCode)[keyof typeof ErrorCode];

export class HotbedError extends Error {
	readonly code: HotbedErrorCode;
	suggestion: string;
	retryable: boolean;

	constructor(
		code: HotbedErrorCode,
		message: string,
		opts?: { suggestion?: string; retryable?: boolean },
	) {
		super(message);
		this.name = "HotbedError";
		this.code = code;
		this.suggestion = opts?.suggestion ?? "";
		this.retryable = opts?.retryable ?? false;
	}

	withSuggestion(suggestion: string): this {
		this.suggestion = suggestion;
		return this;
	}

	toJSON() {
		return {
			code: this.code,
			message: this.message,
			...(this.suggestion ? { suggestion: this.suggestion } : {}),
			retryable: this.retryable,
		};
	}
}

export function hotbedError(
	code: HotbedErrorCode,
	message: string,
	opts?: { suggestion?: string; retryable?: boolean },
): HotbedError {
	return new HotbedError(code, message, opts);
}

export function connectCodeFor(code: HotbedErrorCode): Code {
	switch (code) {
		case ErrorCode.InvalidArgs:
			return Code.InvalidArgument;
		case ErrorCode.NotAuthorized:
			return Code.PermissionDenied;
		case ErrorCode.NotFound:
			return Code.NotFound;
		case ErrorCode.Conflict:
		case ErrorCode.AlreadyInitialized:
			return Code.AlreadyExists;
		case ErrorCode.NotJoined:
		case ErrorCode.ClusterMismatch:
		case ErrorCode.SyncRefused:
		case ErrorCode.NotInitialized:
			return Code.FailedPrecondition;
		case ErrorCode.RepoUnreachable:
		case ErrorCode.WorkerUnreachable:
		case ErrorCode.NodeWaking:
		case ErrorCode.ControllerUnreachable:
		case ErrorCode.TailscaleUnavailable:
			return Code.Unavailable;
		case ErrorCode.PlatformNotSupported:
			return Code.Unimplemented;
		case ErrorCode.CommandStopped:
			return Code.Aborted;
		default:
			return Code.Internal;
	}
}

export function fromUnknownError(err: unknown): HotbedError {
	if (err instanceof HotbedError) {
		return err;
	}
	if (err instanceof ConnectError) {
		for (const d of err.findDetails(ErrorInfoSchema)) {
			return new HotbedError(d.code as HotbedErrorCode, err.rawMessage, {
				suggestion: d.suggestion,
				retryable: d.retryable,
			});
		}
		if (err.code === Code.Unavailable || err.code === Code.DeadlineExceeded) {
			return new HotbedError(ErrorCode.WorkerUnreachable, err.rawMessage, {
				retryable: true,
			});
		}
		if (err.code === Code.PermissionDenied || err.code === Code.Unauthenticated) {
			return new HotbedError(ErrorCode.NotAuthorized, err.rawMessage);
		}
		if (err.code === Code.NotFound) {
			return new HotbedError(ErrorCode.NotFound, err.rawMessage);
		}
		if (err.code === Code.InvalidArgument) {
			return new HotbedError(ErrorCode.InvalidArgs, err.rawMessage);
		}
		if (err.code === Code.AlreadyExists) {
			return new HotbedError(ErrorCode.Conflict, err.rawMessage);
		}
		return new HotbedError(ErrorCode.Internal, err.rawMessage);
	}
	if (err instanceof Error) {
		return new HotbedError(ErrorCode.Internal, err.message);
	}
	return new HotbedError(ErrorCode.Internal, String(err));
}

export function codeOf(err: unknown): HotbedErrorCode | "" {
	if (!err) return "";
	return fromUnknownError(err).code;
}

export function toConnectError(err: unknown): ConnectError {
	if (err instanceof ConnectError && err.code === Code.Unimplemented) {
		return err;
	}
	const he = fromUnknownError(err);
	const detail = create(ErrorInfoSchema, {
		code: he.code,
		suggestion: he.suggestion,
		retryable: he.retryable,
	});
	return new ConnectError(he.message, connectCodeFor(he.code), undefined, [
		{ desc: ErrorInfoSchema, value: detail },
	]);
}

export function fromWorkerError(host: string, err: unknown): HotbedError {
	const he = fromUnknownError(err);
	if (he.code === ErrorCode.ControllerUnreachable) {
		return new HotbedError(ErrorCode.WorkerUnreachable, `${host}: ${he.message}`, {
			retryable: true,
		});
	}
	return he;
}

export const hotbedErrorInterceptor: Interceptor = (next) => async (req) => {
	try {
		return await next(req);
	} catch (err) {
		throw toConnectError(err);
	}
};
