import { describe, expect, it } from "vitest";
import type { Workspace } from "../api";
import { filterWorkspaces } from "./workspaces";

function ws(id: string, over: Partial<Workspace>): Workspace {
  return {
    id, sessionId: "", repoId: "hotbed", baseId: "b", nodeId: "n1", agent: "laptop", branch: "", head: "", filesChanged: 0,
    state: "active", reason: "", createdAt: "2026-11-21T10:00:00Z", forkMillis: 20, exclusiveBytes: 0, sharedBytes: 0, lastActiveAt: "2026-11-21T10:00:00Z",
    ...over,
  };
}

const all = [
  ws("old", { state: "released", lastActiveAt: "2026-11-21T12:00:00Z" }),
  ws("quiet", { repoId: "cladefs", lastActiveAt: "2026-11-21T09:00:00Z", nodeId: "n2" }),
  ws("busy", { sessionId: "ideal-chicken", lastActiveAt: "2026-11-21T11:00:00Z" }),
];
const host = (id: string) => (id === "n2" ? "asahimbp" : "ubu1");

describe("filterWorkspaces", () => {
  it("puts live workspaces first, each group by latest activity", () => {
    expect(filterWorkspaces(all, {}, host).map((w) => w.id)).toEqual(["busy", "quiet", "old"]);
  });

  it("keeps one repo", () => {
    expect(filterWorkspaces(all, { repo: "cladefs" }, host).map((w) => w.id)).toEqual(["quiet"]);
  });

  it("finds a workspace by its session or its node's hostname", () => {
    expect(filterWorkspaces(all, { q: "Chicken" }, host).map((w) => w.id)).toEqual(["busy"]);
    expect(filterWorkspaces(all, { q: "asahi" }, host).map((w) => w.id)).toEqual(["quiet"]);
  });
});
