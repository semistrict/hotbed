import { createConnectTransport } from "@connectrpc/connect-web";
import { commandStreams, createClients } from "./connect";

/**
 * The controller's services over Connect, from the page's own origin: the
 * controller serves this UI and the API side by side.
 */
export const clients = createClients(
  createConnectTransport({ baseUrl: typeof window !== "undefined" ? window.location.origin : "http://127.0.0.1:7120" }),
);

export const { outputBytes, traceCommand, sampleCommand, getCommandDiagnostics } = commandStreams(clients);

export type { Clients, CommandStreams } from "./connect";
export { placeBefore, projectScope } from "./requests";
export * from "./types";
