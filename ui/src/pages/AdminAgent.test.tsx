import { SPEC_DATA_PART_TYPE, type Spec } from "@json-render/core";
import { buildSpecFromParts, JSONUIProvider, Renderer } from "@json-render/react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { adminAgentRegistry } from "../components/AdminAgentRegistry";
import { adminAgentCatalog } from "../lib/adminAgentCatalog";

describe("AdminAgent json-render catalog and registry", () => {
  it("renders a full SQL diagnostic spec with metrics, DataTable, BarList, SqlBlock, and ActionButton", () => {
    const spec: Spec = {
      root: "root-stack",
      elements: {
        "root-stack": {
          type: "Stack",
          props: { direction: "vertical", gap: "md" },
          children: ["kpi-grid", "table-sec", "sql-block", "bars", "action"],
        },
        "kpi-grid": {
          type: "Grid",
          props: { columns: 2, gap: "sm" },
          children: ["m1", "m2"],
        },
        m1: {
          type: "Metric",
          props: { label: "SQLite Size", value: "14.2", unit: "MiB", tone: "ember" },
          children: [],
        },
        m2: {
          type: "Metric",
          props: { label: "Connected Workers", value: 3, tone: "good" },
          children: [],
        },
        "table-sec": {
          type: "Section",
          props: { title: "Controller Durable Object Tables", badge: "ControllerDO", tone: "ember" },
          children: ["tbl"],
        },
        tbl: {
          type: "DataTable",
          props: {
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
          },
          children: [],
        },
        "sql-block": {
          type: "SqlBlock",
          props: {
            sql: "SELECT name FROM sqlite_master WHERE type = 'table'",
            target: "ControllerDO(default)",
            rowCount: 2,
            rowsRead: 2,
            durationMs: 3,
          },
          children: [],
        },
        bars: {
          type: "BarList",
          props: {
            title: "Rows by table",
            items: [{ label: "samples", value: 1420, formattedValue: "1,420", tone: "ember" }],
          },
          children: [],
        },
        action: {
          type: "ActionButton",
          props: { label: "Inspect failures", variant: "outline" },
          children: [],
        },
      },
    };

    const html = renderToStaticMarkup(
      <JSONUIProvider registry={adminAgentRegistry}>
        <Renderer spec={spec} registry={adminAgentRegistry} />
      </JSONUIProvider>,
    );

    expect(html).toContain("SQLite Size");
    expect(html).toContain("14.2");
    expect(html).toContain("Controller Durable Object Tables");
    expect(html).toContain("samples");
    expect(html).toContain("1,420");
    expect(html).toContain("SELECT name FROM sqlite_master");
    expect(html).toContain("Inspect failures");
  });

  it("reconstructs a Spec from AI SDK data-spec parts", () => {
    const parts = [
      { type: "text", text: "Here are the tables." },
      {
        type: SPEC_DATA_PART_TYPE,
        data: {
          type: "patch",
          patch: { op: "add", path: "/root", value: "callout-1" },
        },
      },
      {
        type: SPEC_DATA_PART_TYPE,
        data: {
          type: "patch",
          patch: {
            op: "add",
            path: "/elements/callout-1",
            value: {
              type: "Callout",
              props: { title: "All clear", message: "0 failed commands", tone: "good" },
              children: [],
            },
          },
        },
      },
    ];

    const spec = buildSpecFromParts(parts);
    expect(spec).not.toBeNull();
    expect(spec?.root).toBe("callout-1");
    expect(Object.keys(adminAgentCatalog.data.components)).toContain("DataTable");
  });
});
