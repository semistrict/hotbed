import { createConnectApi } from "./connect";
import type { Api } from "./types";

/** The controller's API, over Connect. */
export const api: Api = createConnectApi();

export * from "./types";
