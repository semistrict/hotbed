import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { DataTableView, SqlBlockView } from "../components/AdminSqlViews";
import { DoLinkContext } from "../lib/doLinks";

describe("Admin Explorer SQL views", () => {
  it("renders formatted SQL blocks and sortable result tables", () => {
    const html = renderToStaticMarkup(
      <div>
        <SqlBlockView
          props={{
            sql: "SELECT name FROM sqlite_master WHERE type = 'table'",
            target: "CONTROLLER(default)",
            rowCount: 2,
            rowsRead: 2,
            durationMs: 3,
          }}
        />
        <DataTableView
          props={{
            title: "Top tables",
            columns: [
              { key: "name", label: "Table", format: "code" },
              { key: "rows", label: "Rows", format: "number", align: "right" },
              { key: "size", label: "Size", format: "bytes", align: "right" },
              { key: "status", label: "Status", format: "badge" },
            ],
            rows: [
              { name: "samples", rows: 1420, size: 2048, status: "healthy" },
              { name: "commands", rows: 85, size: 1024, status: "failed" },
            ],
          }}
        />
      </div>,
    );

    expect(html).toContain("CONTROLLER(default)");
    expect(html).toContain("SELECT name FROM sqlite_master");
    expect(html).toContain("samples");
    expect(html).toContain("1,420");
    expect(html).toContain("healthy");
  });

  it("renders clickable Durable Object link chips for columns referencing other DOs", () => {
    const html = renderToStaticMarkup(
      <DoLinkContext.Provider
        value={{
          scope: {
            controllerDoName: "default",
            orgId: null,
            clusterId: "clu-42",
            cladefsTenantId: 0,
          },
          allowedBindings: ["CONTROLLER", "COMMAND"],
          onOpenDoTarget: () => {},
        }}
      >
        <DataTableView
          props={{
            title: "CONTROLLER(default)",
            columns: [
              { key: "id", label: "id", format: "code" },
              { key: "node_id", label: "node_id", format: "code" },
            ],
            rows: [{ id: "cmd-99", node_id: "ubu1" }],
          }}
          linkContext={{
            currentBinding: "CONTROLLER",
            currentTargetName: "default",
            sql: 'SELECT * FROM "commands" LIMIT 50',
          }}
        />
      </DoLinkContext.Provider>,
    );

    expect(html).toContain('title="Open COMMAND(cmd:clu-42:cmd-99) in SQL Explorer"');
    expect(html).toContain("cmd-99");
    expect(html).not.toContain('title="Open COMMAND(cmd:clu-42:ubu1)');
  });
});
