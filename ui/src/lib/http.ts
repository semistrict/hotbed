/**
 * Fetches JSON from the controller's own HTTP routes (/auth/, /admin/, /api/),
 * with the session cookie. A body is JSON unless the caller says otherwise.
 * A failed request throws the error or message the controller sent back.
 */
export async function fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("Accept", "application/json");
  if (init?.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const res = await fetch(path, { ...init, headers, credentials: "same-origin" });
  const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
  if (!res.ok) {
    const msg = typeof data.error === "string" ? data.error : typeof data.message === "string" ? data.message : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return data as T;
}
