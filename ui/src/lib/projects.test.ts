import { describe, expect, it } from "vitest";
import type { Snapshot } from "../api";
import { remoteName, scope } from "./projects";

const stamp = "2026-11-21T12:00:00Z";
const s = {
  projects: [
    { name: "ratel", repos: ["github.com/o/lnx", "github.com/o/ratel"], createdAt: stamp, liveSessions: 1, openQuestions: 2, outbox: 0 },
    { name: "hotbed", repos: ["github.com/o/hotbed"], createdAt: stamp, liveSessions: 1, openQuestions: 1, outbox: 1 },
  ],
  repos: [
    { id: "ratel", remote: "github.com/o/ratel" },
    { id: "lnx", remote: "github.com/o/lnx" },
    { id: "hotbed", remote: "github.com/o/hotbed" },
  ],
  bases: [{ id: "lnx-g1", repoId: "lnx" }, { id: "hotbed-g4", repoId: "hotbed" }],
  sessions: [{ id: "brave-otter", project: "ratel" }, { id: "calm-heron", project: "hotbed" }],
  workspaces: [{ id: "w1", sessionId: "brave-otter" }, { id: "w2", sessionId: "calm-heron" }],
  commands: [{ id: "c1", sessionId: "brave-otter" }, { id: "c2", sessionId: "calm-heron" }],
  history: [
    { id: "h1", sessionId: "brave-otter", repoId: "ratel" },
    { id: "h2", sessionId: "calm-heron", repoId: "hotbed" },
    { id: "f1", sessionId: "", repoId: "lnx" },
    { id: "f2", sessionId: "", repoId: "hotbed" },
  ],
  schedules: [
    { name: "lnx-nightly", repoId: "lnx", project: null },
    { name: "hotbed-soak", repoId: "hotbed", project: "hotbed" },
    { name: "lnx-in-hotbed", repoId: "lnx", project: "hotbed" },
  ],
  events: [
    { id: 1, ref: { type: "command", id: "c1" } },
    { id: 2, ref: { type: "workspace", id: "w2" } },
    { id: 3, ref: { type: "command", id: "f1" } },
    { id: 4, ref: { type: "node", id: "nUbu1" } },
    { id: 5, ref: null },
  ],
  nodes: [{ id: "nUbu1" }],
  openQuestions: 3,
} as unknown as Snapshot;

const ids = (xs: { id: string | number }[]) => xs.map((x) => x.id);

describe("scope", () => {
  it("keeps a project's sessions, what they run, and what its repos have", () => {
    const r = scope(s, "ratel");
    expect(ids(r.sessions)).toEqual(["brave-otter"]);
    expect(ids(r.workspaces)).toEqual(["w1"]);
    expect(ids(r.commands)).toEqual(["c1"]);
    expect(ids(r.history)).toEqual(["h1", "f1"]);
    expect(ids(r.repos)).toEqual(["ratel", "lnx"]);
    expect(ids(r.bases)).toEqual(["lnx-g1"]);
    expect(r.schedules.map((sc) => sc.name)).toEqual(["lnx-nightly"]);
    expect(scope(s, "hotbed").schedules.map((sc) => sc.name)).toEqual(["hotbed-soak", "lnx-in-hotbed"]);
    expect(ids(r.events)).toEqual([1, 3]);
    expect(r.openQuestions).toBe(2);
  });
  it("leaves the machines whole", () => {
    expect(scope(s, "hotbed").nodes).toBe(s.nodes);
  });
  it("is every project for none, or one it does not know", () => {
    expect(scope(s, null)).toBe(s);
    expect(scope(s, "gone")).toBe(s);
  });
});

describe("remoteName", () => {
  it("drops the host", () => {
    expect(remoteName("github.com/semistrict/hotbed")).toBe("semistrict/hotbed");
    expect(remoteName("/srv/git/app")).toBe("srv/git/app");
  });
});
