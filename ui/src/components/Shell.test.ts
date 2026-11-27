import { describe, expect, it } from "vitest";
import { isNavActive, routeParent, routeTitle } from "./Shell";

describe("Shell navigation helpers", () => {
  it("marks Projects active for root and workspace detail routes", () => {
    expect(isNavActive("/", "/")).toBe(true);
    expect(isNavActive("/workspaces/ws-123", "/")).toBe(true);
    expect(isNavActive("/questions", "/")).toBe(false);
    expect(isNavActive("/nodes/ubu1", "/")).toBe(false);
  });

  it("marks section routes active for both list and detail deep links", () => {
    expect(isNavActive("/questions", "/questions")).toBe(true);
    expect(isNavActive("/memories", "/memories")).toBe(true);
    expect(isNavActive("/memories/42", "/memories")).toBe(true);
    expect(isNavActive("/skills", "/skills")).toBe(true);
    expect(isNavActive("/skills/42", "/skills")).toBe(true);
    expect(isNavActive("/reviews/7", "/reviews")).toBe(true);
    expect(isNavActive("/sessions/bold-otter", "/sessions")).toBe(true);
    expect(isNavActive("/commands/cmd-9", "/commands")).toBe(true);
    expect(isNavActive("/nodes/ubu1.local", "/nodes")).toBe(true);
    expect(isNavActive("/logs", "/logs")).toBe(true);
    expect(isNavActive("/settings", "/settings")).toBe(true);
    expect(isNavActive("/org", "/org")).toBe(true);
    expect(isNavActive("/cluster", "/cluster")).toBe(true);
    expect(isNavActive("/admin-explorer", "/admin-explorer")).toBe(true);
  });

  it("does not match sibling prefixes accidentally", () => {
    expect(isNavActive("/organizations", "/org")).toBe(false);
    expect(isNavActive("/clusters", "/cluster")).toBe(false);
  });

  it("resolves human-readable header titles for every deep link", () => {
    expect(routeTitle("/")).toBe("Projects");
    expect(routeTitle("/workspaces/ws-1")).toBe("Workspace");
    expect(routeTitle("/reviews/42")).toBe("Reviews");
    expect(routeTitle("/memories/11")).toBe("Memories");
    expect(routeTitle("/skills/11")).toBe("Skills");
    expect(routeTitle("/nodes/ubu1")).toBe("Nodes");
    expect(routeTitle("/commands/abc")).toBe("Commands");
    expect(routeTitle("/sessions/warm-fox")).toBe("Sessions");
    expect(routeTitle("/settings")).toBe("Settings");
    expect(routeTitle("/admin-explorer")).toBe("Admin Explorer");
    expect(routeTitle("/org")).toBe("Organization");
    expect(routeTitle("/org", false)).toBe("Access & Tokens");
  });

  it("resolves section root links for detail routes and null for section roots", () => {
    expect(routeParent("/nodes/ubu1")).toBe("/nodes");
    expect(routeParent("/sessions/warm-fox")).toBe("/sessions");
    expect(routeParent("/commands/abc")).toBe("/commands");
    expect(routeParent("/reviews/42")).toBe("/reviews");
    expect(routeParent("/memories/11")).toBe("/memories");
    expect(routeParent("/skills/11")).toBe("/skills");
    expect(routeParent("/workspaces/ws-1")).toBe("/");

    expect(routeParent("/")).toBeNull();
    expect(routeParent("/nodes")).toBeNull();
    expect(routeParent("/sessions")).toBeNull();
    expect(routeParent("/commands")).toBeNull();
    expect(routeParent("/reviews")).toBeNull();
    expect(routeParent("/memories")).toBeNull();
    expect(routeParent("/org")).toBeNull();
    expect(routeParent("/cluster")).toBeNull();
    expect(routeParent("/logs")).toBeNull();
  });
});
