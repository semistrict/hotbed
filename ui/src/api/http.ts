import type { Api, CommandLog, FileContent, FileEntry, Snapshot } from "./types";

/** The controller's error body, the same one the CLI prints in its envelope. */
export interface ApiError {
  code: string;
  message: string;
  suggestion?: string;
  retryable: boolean;
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as ApiError | null;
    throw new Error(body ? `${body.code}: ${body.message}` : `HTTP ${res.status} from ${path}`);
  }
  return (await res.json()) as T;
}

/** The controller's /v1 API, served by `hotbed run` beside this UI. */
export function createHttpApi(): Api {
  return {
    snapshot: () => get<Snapshot>("/v1/ui/snapshot"),
    commandLog: (id) => get<CommandLog>(`/v1/commands/${encodeURIComponent(id)}/log`),
    files: (ws) => get<FileEntry[]>(`/v1/workspaces/${encodeURIComponent(ws)}/files`),
    file: (ws, path) => get<FileContent>(`/v1/workspaces/${encodeURIComponent(ws)}/files/${path.split("/").map(encodeURIComponent).join("/")}`),
  };
}
