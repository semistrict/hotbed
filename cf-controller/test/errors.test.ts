import { Code, ConnectError } from "@connectrpc/connect";
import { errorFromJson } from "@connectrpc/connect/protocol-connect";
import { describe, expect, it } from "vite-plus/test";
import { connectErrorResponse, ErrorCode, hotbedError } from "../src/errors.ts";
import { ErrorInfoSchema } from "../src/gen/hotbed/v1/hotbed_pb.ts";

describe("connectErrorResponse", () => {
	it.each([
		[ErrorCode.NotAuthorized, 403, Code.PermissionDenied],
		[ErrorCode.InvalidArgs, 400, Code.InvalidArgument],
		[ErrorCode.NotFound, 404, Code.NotFound],
		[ErrorCode.ControllerUnreachable, 503, Code.Unavailable],
		[ErrorCode.WorkerUnreachable, 503, Code.Unavailable],
		[ErrorCode.Internal, 500, Code.Internal],
	])(
		"answers %s with HTTP %i and the Connect code the router would",
		async (code, status, want) => {
			const res = connectErrorResponse(
				hotbedError(code, "it went wrong", { suggestion: "try again", retryable: true }),
				{ "Hotbed-Build": "7" },
			);
			expect(res.status).toBe(status);
			expect(res.headers.get("Content-Type")).toBe("application/json");
			expect(res.headers.get("Hotbed-Build")).toBe("7");
			const ce = errorFromJson(
				await res.json(),
				undefined,
				new ConnectError("not a Connect error"),
			);
			expect(ce.code).toBe(want);
			expect(ce.rawMessage).toBe("it went wrong");
			const [info] = ce.findDetails(ErrorInfoSchema);
			expect({ code: info.code, suggestion: info.suggestion, retryable: info.retryable }).toEqual({
				code,
				suggestion: "try again",
				retryable: true,
			});
		},
	);
});
