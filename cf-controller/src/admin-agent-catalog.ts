// Shared Vercel json-render catalog definition for the Hotbed Admin Agent.
// Uses @json-render/core directly (zero React dependency in the Cloudflare Worker)
// with the exact same element-tree schema as @json-render/react/schema so specs
// streamed via pipeJsonRender() render natively with @json-render/react in ui/.

import { defineCatalog, defineSchema } from "@json-render/core";
import { z } from "zod";

const elementTreeSchema = defineSchema(
	(s) => ({
		spec: s.object({
			root: s.string(),
			elements: s.record(
				s.object({
					type: s.ref("catalog.components"),
					props: s.propsOf("catalog.components"),
					children: s.array(s.string()),
					slots: { ...s.record(s.array(s.string())), ...s.optional() },
					visible: { ...s.any(), ...s.optional() },
					repeat: { ...s.any(), ...s.optional() },
				}),
			),
		}),
		catalog: s.object({
			components: s.map({
				props: s.zod(),
				slots: s.array(s.string()),
				description: s.string(),
				example: s.any(),
			}),
			actions: s.map({
				params: s.zod(),
				description: s.string(),
			}),
		}),
	}),
	{
		defaultRules: [
			"CRITICAL INTEGRITY CHECK: Before outputting ANY element that references children, you MUST output every child as its own element. Every key in every children array must resolve to a defined element.",
			'REQUIRED FIELDS: Every element MUST include a "children" array. Leaf elements use "children": [].',
			'CRITICAL: Only put interactive events in "on" (e.g. on.press) on the element object, never inside "props".',
			"NEVER invent or fabricate data. Every number, row, table name, and status in the UI spec MUST come directly from your tool outputs.",
		],
	},
);

const toneSchema = z.enum(["default", "ember", "frost", "good", "warning", "critical"]).optional();

export const adminAgentCatalog = defineCatalog(elementTreeSchema, {
	components: {
		Stack: {
			props: z.object({
				direction: z.enum(["vertical", "horizontal"]).optional(),
				gap: z.enum(["sm", "md", "lg"]).optional(),
				wrap: z.boolean().optional(),
			}),
			slots: ["default"],
			description: "Flex layout container to arrange child components vertically or horizontally.",
			example: { direction: "vertical", gap: "md" },
		},
		Grid: {
			props: z.object({
				columns: z.union([z.literal(1), z.literal(2), z.literal(3), z.literal(4)]).optional(),
				gap: z.enum(["sm", "md"]).optional(),
			}),
			slots: ["default"],
			description: "Responsive multi-column grid for KPI Metrics, cards, or side-by-side panels.",
			example: { columns: 4, gap: "sm" },
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
			example: {
				title: "Controller Durable Object Tables",
				subtitle: "default · 14.2 MiB SQLite",
				badge: "ControllerDO",
				tone: "ember",
			},
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
			description: "Single headline KPI tile (count, size, duration, latency, or rate).",
			example: {
				label: "SQLite Size",
				value: "14.2",
				unit: "MiB",
				detail: "28 tables",
				tone: "ember",
			},
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
			description:
				"Formatted, sortable data table for SQL query results and tabular records. Supports bytes, durationMs, timestamp (Unix ms), code, badge, and number formatting.",
			example: {
				title: "Largest tables",
				columns: [
					{ key: "name", label: "Table", format: "code" },
					{ key: "rows", label: "Rows", align: "right", format: "number" },
				],
				rows: [{ name: "samples", rows: 1420 }],
				searchable: true,
			},
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
			description:
				"Horizontal proportional bar list for rankings, breakdowns, storage shares, or failure counts.",
			example: {
				title: "Commands by node",
				items: [{ label: "ubu1", value: 42, formattedValue: "42 runs", tone: "ember" }],
			},
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
			description:
				"Compact key-value metadata grid for inspecting a single record, health status, or object.",
			example: {
				items: [
					{ key: "Cluster ID", value: "abc123", mono: true },
					{ key: "Connected Workers", value: 3, badge: true, tone: "good" },
				],
			},
		},
		Callout: {
			props: z.object({
				title: z.string(),
				message: z.string(),
				tone: z.enum(["info", "good", "warning", "critical"]),
			}),
			slots: [],
			description: "Highlighted alert box for anomalies, warnings, errors, or key takeaways.",
			example: {
				title: "No failed commands",
				message: "All 18 commands in the last 24 hours exited cleanly.",
				tone: "good",
			},
		},
		Badge: {
			props: z.object({
				label: z.string(),
				tone: toneSchema,
			}),
			slots: [],
			description: "Inline status badge.",
			example: { label: "healthy", tone: "good" },
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
			description:
				"Displays an executed SQL query alongside its target Durable Object and execution metrics.",
			example: {
				sql: "SELECT id, hostname, last_seen FROM nodes ORDER BY last_seen DESC",
				target: "ControllerDO (default)",
				rowCount: 3,
				rowsRead: 3,
				durationMs: 2,
			},
		},
		CodeBlock: {
			props: z.object({
				title: z.string().optional(),
				language: z.string().optional(),
				code: z.string(),
				collapsible: z.boolean().optional(),
			}),
			slots: [],
			description: "Monospace code or JSON viewer with copy support.",
			example: { title: "Schema DDL", language: "sql", code: "CREATE TABLE ..." },
		},
		Text: {
			props: z.object({
				content: z.string(),
				variant: z.enum(["body", "muted", "mono", "heading"]).optional(),
			}),
			slots: [],
			description: "Text block inside a Section or Stack.",
			example: { content: "Showing top 20 commands by duration.", variant: "muted" },
		},
		ActionButton: {
			props: z.object({
				label: z.string(),
				variant: z.enum(["default", "secondary", "outline", "ghost"]).optional(),
			}),
			slots: [],
			description:
				"Interactive button. Attach an `on.press` action (`askFollowUp`, `navigateTo`, or `copyText`) on the element.",
			example: { label: "Inspect failures", variant: "outline" },
		},
	},
	actions: {
		askFollowUp: {
			params: z.object({ prompt: z.string() }),
			description: "Send a follow-up question to the Admin Agent.",
		},
		navigateTo: {
			params: z.object({ path: z.string() }),
			description:
				"Navigate inside the Hotbed Cockpit (e.g. /commands/<id>, /sessions/<id>, /nodes/<hostname>, /cluster, /questions, /logs).",
		},
		copyText: {
			params: z.object({ text: z.string() }),
			description: "Copy text (such as a SQL query or ID) to the clipboard.",
		},
	},
});

export function buildAdminAgentSystemPrompt(opts: {
	cloudflareApiConfigured: boolean;
	gcWorkerConfigured: boolean;
}): string {
	const baseIntro = `You are the Hotbed Admin Agent, an expert Cloudflare Durable Objects and infrastructure inspector running inside the hotbed-controller Cloudflare Worker.

## Your Architecture & Access
You have direct typed Workers RPC access to ALL SQLite Cloudflare Durable Objects in this project:
1. **ControllerDO** (\`binding: "CONTROLLER"\`): One SQLite Durable Object per organization (\`name: "default"\` for the default/owner org, or \`name: "org:<org_id>"\`, or any 64-hex-char ID). Stores nodes, projects, sessions, workspaces, commands, history, samples, events, placements, schedules, questions, issues, memories, reviews, board_messages, daemon_logs, shared_bases, worker_configs, api_tokens, join_tokens, worker_tokens, signup_codes, kv_meta, base_freshens.
   - Important: All timestamps (\`created_at\`, \`started_at\`, \`ended_at\`, \`last_seen\`, \`at\`, \`t\`, \`updated_at\`) are **Unix epoch milliseconds** (integers).
   - Many tables store protobuf JSON in a \`doc\` column (use SQLite \`json_extract(doc, '$.field')\` to query structured fields cleanly!).
   - Never expose secret values (\`secret\`, \`token_hash\`) verbatim in responses.
2. **TenantRegistryDO** (\`binding: "TENANT_REGISTRY"\`, \`name: "global"\`): Global mapping of every Hotbed organization (\`org_tenants\` table: \`org_key\`, \`tenant_id\`, \`created_at\`) to its permanent CladeFS uint64 tenant ID.
3. **CommandDO** (\`binding: "COMMAND"\`, \`name: "cmd:<clusterId>:<commandId>"\` or 64-hex-char ID): Per-command SQLite Durable Object storing heavy per-command data: \`command_meta\`, \`command_profile\`, \`command_diagnostics\`, \`command_samples\`, \`command_output\`, \`command_output_chunks\`. To find a command's DO name, query \`kv_meta\` (\`key = 'cluster_id'\`) and \`commands\` / \`history\` in \`ControllerDO\`, then address \`CommandDO\` with \`name: "cmd:<clusterId>:<commandId>"\`.
4. **AdminAgentDO** (\`binding: "ADMIN_AGENT"\`): Your own Durable Object SQLite database storing chat threads and messages.

Additional live integrations:
- **CladeFS GC Worker**: ${opts.gcWorkerConfigured ? "CONFIGURED (use `inspect_gc_worker` to inspect R2 cell usage, health, and GC dry-runs)." : "Not configured in this environment."}
- **Cloudflare Account REST API**: ${opts.cloudflareApiConfigured ? "CONFIGURED (use `list_cf_do_objects`, `query_cf_observability`, and `cf_api_get` for account-wide DO listing, Workers deployments, R2, and Workers Observability telemetry)." : "Not configured (CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID not set). You can still discover and run SQL on all in-cluster Durable Objects via `discover_durable_objects`, `inspect_do_schema`, and `run_do_sql`."}

## Working Style
- Always inspect real data with tools before answering — never guess counts, table names, or schemas.
- If you aren't sure about a table's columns, call \`inspect_do_schema\` first, then run targeted \`run_do_sql\` queries.
- All SQL queries are strictly read-only (\`SELECT\`, \`WITH ... SELECT\`, \`EXPLAIN\`, read-only \`PRAGMA\`).
- In user-facing prose and titles, say "Cloudflare Durable Object" rather than bare "DO".
- Be concise and direct. Start with a crisp 1-3 sentence summary of the answer, then present structured results using a json-render \`\`\`spec\`\`\` block!
- Whenever you run SQL or return tabular/numeric/diagnostic results, emit a clean \`\`\`spec\`\`\` block using the catalog below:
  - Put headline counts/sizes in a \`Grid\` of \`Metric\` tiles.
  - Put SQL rows in a \`DataTable\` with appropriate column \`format\` (\`bytes\`, \`durationMs\`, \`timestamp\`, \`number\`, \`code\`, \`badge\`).
  - Put rankings/distributions in a \`BarList\`.
  - Include a \`SqlBlock\` showing the key SQL query you ran so the admin can verify or copy it.
  - Optionally include 1-3 helpful \`ActionButton\`s with \`on.press\` (\`askFollowUp\` or \`navigateTo\`) for natural next steps!`;

	return adminAgentCatalog.prompt({
		mode: "inline",
		system: baseIntro,
		customRules: [
			"Always write a concise prose takeaway before the ```spec fence, and keep the ```spec JSONL patches clean and valid.",
			"When displaying tabular SQL results, prefer DataTable over Markdown tables so the user gets sorting, search, and clean formatting.",
			"For timestamps in DataTable columns (Unix epoch ms), set format: 'timestamp'. For byte counts, set format: 'bytes'. For millisecond durations, set format: 'durationMs'.",
			"Never leak secrets such as git_credentials.secret or token_hash columns.",
		],
	});
}
