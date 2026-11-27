import { describe, expect, it } from "vitest";
import type { AdminOrgScopeInfo, DoBindingName } from "./adminAgent";
import { extractSingleFromTable, formatDoTargetLabel, resolveCellDoLink } from "./doLinks";

const defaultScope: AdminOrgScopeInfo = {
  controllerDoName: "default",
  orgId: null,
  clusterId: "cluster-abc",
  cladefsTenantId: 0,
};

const alphaScope: AdminOrgScopeInfo = {
  controllerDoName: "org:org_alpha",
  orgId: "org_alpha",
  clusterId: "cluster-alpha",
  cladefsTenantId: 7,
};

const orgBindings: DoBindingName[] = ["CONTROLLER", "COMMAND"];

describe("extractSingleFromTable", () => {
  it("extracts table names from quoted and unquoted SELECT queries", () => {
    expect(extractSingleFromTable('SELECT * FROM "commands" LIMIT 50')).toBe("commands");
    expect(extractSingleFromTable("SELECT id, node_id FROM history ORDER BY ended_at DESC")).toBe("history");
    expect(extractSingleFromTable("SELECT * FROM main.commands WHERE id = 'x'")).toBe("commands");
  });

  it("returns null for JOIN queries where bare id could be ambiguous", () => {
    expect(extractSingleFromTable("SELECT c.id FROM commands c JOIN sessions s ON s.id = c.session_id")).toBeNull();
  });
});

describe("resolveCellDoLink", () => {
  it("links commands.id and history.id on ControllerDO to CommandDO", () => {
    const cmdLink = resolveCellDoLink("cmd-123", "id", { id: "cmd-123" }, {
      scope: defaultScope,
      allowedBindings: orgBindings,
      currentBinding: "CONTROLLER",
      currentTargetName: "default",
      sql: 'SELECT * FROM "commands" LIMIT 50',
    });
    expect(cmdLink).toEqual({ binding: "COMMAND", name: "cmd:cluster-abc:cmd-123" });

    const histLink = resolveCellDoLink("cmd-456", "id", { id: "cmd-456" }, {
      scope: defaultScope,
      allowedBindings: orgBindings,
      currentBinding: "CONTROLLER",
      currentTargetName: "default",
      sql: "SELECT id, ended_at FROM history LIMIT 20",
    });
    expect(histLink).toEqual({ binding: "COMMAND", name: "cmd:cluster-abc:cmd-456" });
  });

  it("does NOT link non-DO id columns like projects.id, nodes.id, or sessions.id", () => {
    for (const table of ["projects", "nodes", "sessions", "workspaces"]) {
      const res = resolveCellDoLink("row-1", "id", { id: "row-1" }, {
        scope: defaultScope,
        allowedBindings: orgBindings,
        currentBinding: "CONTROLLER",
        currentTargetName: "default",
        sql: `SELECT * FROM "${table}" LIMIT 50`,
      });
      expect(res).toBeNull();
    }
  });

  it("links schedule_runs.command_id and events.ref_id (when ref_type=command)", () => {
    expect(
      resolveCellDoLink("cmd-77", "command_id", { command_id: "cmd-77" }, {
        scope: defaultScope,
        allowedBindings: orgBindings,
        currentBinding: "CONTROLLER",
        currentTargetName: "default",
        sql: 'SELECT * FROM "schedule_runs" LIMIT 50',
      }),
    ).toEqual({ binding: "COMMAND", name: "cmd:cluster-abc:cmd-77" });

    expect(
      resolveCellDoLink("cmd-88", "ref_id", { ref_type: "command", ref_id: "cmd-88" }, {
        scope: defaultScope,
        allowedBindings: orgBindings,
        currentBinding: "CONTROLLER",
        currentTargetName: "default",
        sql: 'SELECT * FROM "events" LIMIT 50',
      }),
    ).toEqual({ binding: "COMMAND", name: "cmd:cluster-abc:cmd-88" });

    expect(
      resolveCellDoLink("ubu1", "ref_id", { ref_type: "node", ref_id: "ubu1" }, {
        scope: defaultScope,
        allowedBindings: orgBindings,
        currentBinding: "CONTROLLER",
        currentTargetName: "default",
        sql: 'SELECT * FROM "events" LIMIT 50',
      }),
    ).toBeNull();
  });

  it("links explicit cmd: DO names strictly within the active org's cluster", () => {
    expect(
      resolveCellDoLink("cmd:cluster-alpha:run-9", "do_name", {}, {
        scope: alphaScope,
        allowedBindings: orgBindings,
      }),
    ).toEqual({ binding: "COMMAND", name: "cmd:cluster-alpha:run-9" });
  });

  it("enforces per-organization scope and suppresses cross-org links", () => {
    expect(
      resolveCellDoLink("cmd:cluster-beta:run-1", "do_name", {}, {
        scope: alphaScope,
        allowedBindings: orgBindings,
      }),
    ).toBeNull();

    expect(
      resolveCellDoLink("org:org_beta", "controller_do_name", {}, {
        scope: alphaScope,
        allowedBindings: orgBindings,
      }),
    ).toBeNull();
  });

  it("skips self-links when already inspecting the same Durable Object", () => {
    expect(
      resolveCellDoLink("default", "controller_do_name", {}, {
        scope: defaultScope,
        allowedBindings: orgBindings,
        currentBinding: "CONTROLLER",
        currentTargetName: "default",
      }),
    ).toBeNull();
  });

  it("links 64-character hex Durable Object IDs with explicit binding columns", () => {
    const hex = "a".repeat(64);
    expect(
      resolveCellDoLink(hex, "do_id", { binding: "COMMAND" }, {
        scope: defaultScope,
        allowedBindings: orgBindings,
      }),
    ).toEqual({ binding: "COMMAND", id: hex });
    expect(formatDoTargetLabel({ binding: "COMMAND", id: hex })).toBe("COMMAND(id:aaaaaaaaaaaa…)");
  });
});
