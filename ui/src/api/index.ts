import { createHttpApi } from "./http";
import { createMockApi } from "./mock";
import type { Api } from "./types";

/**
 * The UI runs against a simulated cluster until the controller serves the
 * same API; build with VITE_HOTBED_API=http to use the controller.
 */
export const api: Api = import.meta.env.VITE_HOTBED_API === "http" ? createHttpApi() : createMockApi();

export const usingMock = import.meta.env.VITE_HOTBED_API !== "http";

export type * from "./types";
