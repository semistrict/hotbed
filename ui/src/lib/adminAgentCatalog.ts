import { defineCatalog } from "@json-render/core";
import { schema } from "@json-render/react/schema";
import { z } from "zod";

const toneSchema = z
  .enum(["default", "ember", "frost", "good", "warning", "critical"])
  .optional();

/**
 * Frontend catalog matching cf-controller/src/admin-agent-catalog.ts.
 * Every component in this catalog is rendered safely by AdminAgentRegistry.tsx.
 */
export const adminAgentCatalog = defineCatalog(schema, {
  components: {
    Stack: {
      props: z.object({
        direction: z.enum(["vertical", "horizontal"]).optional(),
        gap: z.enum(["sm", "md", "lg"]).optional(),
        wrap: z.boolean().optional(),
      }),
      slots: ["default"],
      description: "Flex layout container to arrange child components vertically or horizontally.",
    },
    Grid: {
      props: z.object({
        columns: z.union([z.literal(1), z.literal(2), z.literal(3), z.literal(4)]).optional(),
        gap: z.enum(["sm", "md"]).optional(),
      }),
      slots: ["default"],
      description: "Responsive multi-column grid for KPI Metrics, cards, or side-by-side panels.",
    },
    Section: {
      props: z.object({
        title: z.string(),
        subtitle: z.string().optional(),
        badge: z.string().optional(),
        tone: toneSchema,
      }),
      slots: ["default"],
      description: "Card panel with a title, optional subtitle, and optional status badge.",
    },
    Metric: {
      props: z.object({
        label: z.string(),
        value: z.union([z.string(), z.number()]),
        unit: z.string().optional(),
        detail: z.string().optional(),
        tone: toneSchema,
      }),
      slots: [],
      description: "Single headline KPI tile.",
    },
    DataTable: {
      props: z.object({
        title: z.string().optional(),
        caption: z.string().optional(),
        columns: z.array(
          z.object({
            key: z.string(),
            label: z.string(),
            align: z.enum(["left", "right"]).optional(),
            format: z
              .enum(["text", "number", "bytes", "durationMs", "timestamp", "code", "badge", "json"])
              .optional(),
          }),
        ),
        rows: z.array(z.record(z.string(), z.unknown())),
        searchable: z.boolean().optional(),
        maxHeight: z.number().optional(),
      }),
      slots: [],
      description: "Formatted, sortable data table for SQL query results.",
    },
    BarList: {
      props: z.object({
        title: z.string().optional(),
        items: z.array(
          z.object({
            label: z.string(),
            value: z.number(),
            formattedValue: z.string().optional(),
            detail: z.string().optional(),
            tone: toneSchema,
          }),
        ),
        max: z.number().optional(),
      }),
      slots: [],
      description: "Horizontal proportional bar list for rankings and distributions.",
    },
    KeyValueList: {
      props: z.object({
        items: z.array(
          z.object({
            key: z.string(),
            value: z.union([z.string(), z.number(), z.boolean(), z.null()]),
            mono: z.boolean().optional(),
            badge: z.boolean().optional(),
            tone: toneSchema,
          }),
        ),
      }),
      slots: [],
      description: "Compact key-value metadata grid.",
    },
    Callout: {
      props: z.object({
        title: z.string(),
        message: z.string(),
        tone: z.enum(["info", "good", "warning", "critical"]),
      }),
      slots: [],
      description: "Highlighted alert box.",
    },
    Badge: {
      props: z.object({
        label: z.string(),
        tone: toneSchema,
      }),
      slots: [],
      description: "Inline status badge.",
    },
    SqlBlock: {
      props: z.object({
        sql: z.string(),
        target: z.string().optional(),
        rowCount: z.number().optional(),
        rowsRead: z.number().optional(),
        durationMs: z.number().optional(),
        truncated: z.boolean().optional(),
      }),
      slots: [],
      description: "Executed SQL query display with target and stats.",
    },
    CodeBlock: {
      props: z.object({
        title: z.string().optional(),
        language: z.string().optional(),
        code: z.string(),
        collapsible: z.boolean().optional(),
      }),
      slots: [],
      description: "Monospace code or JSON viewer.",
    },
    Text: {
      props: z.object({
        content: z.string(),
        variant: z.enum(["body", "muted", "mono", "heading"]).optional(),
      }),
      slots: [],
      description: "Text block.",
    },
    ActionButton: {
      props: z.object({
        label: z.string(),
        variant: z.enum(["default", "secondary", "outline", "ghost"]).optional(),
      }),
      slots: [],
      description: "Interactive button bound to askFollowUp, navigateTo, or copyText.",
    },
  },
  actions: {
    askFollowUp: {
      params: z.object({ prompt: z.string() }),
      description: "Send a follow-up question to the Admin Agent.",
    },
    navigateTo: {
      params: z.object({ path: z.string() }),
      description: "Navigate inside the Hotbed Cockpit.",
    },
    copyText: {
      params: z.object({ text: z.string() }),
      description: "Copy text to the clipboard.",
    },
  },
});
