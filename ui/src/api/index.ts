import { createConnectApi } from "./connect";
import { createMockApi } from "./mock";
import type { Api } from "./types";

/**
 * The controller's API, over Connect. `pnpm dev` runs against a simulated
 * cluster instead (VITE_HOTBED_API=mock), so the UI can be worked on
 * without one.
 */
export const usingMock = import.meta.env.VITE_HOTBED_API === "mock";

export const api: Api = usingMock ? createMockApi() : createConnectApi();

export type * from "./types";
