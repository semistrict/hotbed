import { env, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vite-plus/test";
import migrations from "../drizzle/migrations.js";
import type { ControllerEnv } from "../src/controller-do.ts";
import { str } from "../src/sql-values.ts";
import type { ControllerStore } from "../src/store/index.ts";

const inController = <T>(fn: (store: ControllerStore) => T) =>
	runInDurableObject(
		(env as unknown as ControllerEnv).CONTROLLER.getByName("memory-migration"),
		(instance) => fn((instance as unknown as { store: ControllerStore }).store),
	);

// 0025 dropped memory modes; its last statement turns each stored memory into
// a summary and a body.
const backfill = migrations.migrations.m0025.split("--> statement-breakpoint").at(-1)!;

describe("migration 0025: memories without a mode", () => {
	it("gives a former always-on rule its summary as its body and drops every mode", async () => {
		const docs = await inController((store) => {
			const legacy = (slug: string, content: Record<string, unknown>) =>
				store.kernel.insert(
					`INSERT INTO memories (scope_key, slug, state, proposed_at, updated_at, doc)
					 VALUES ('', ?, 'MEMORY_STATE_APPROVED', 1, 1, ?)`,
					slug,
					JSON.stringify({ scope: "MEMORY_SCOPE_GLOBAL", content: { slug, ...content } }),
				);
			const rule = legacy("legacy-rule", {
				summary: "Bind services to 127.0.0.1.",
				mode: "MEMORY_MODE_ALWAYS",
			});
			const ruleWithBody = legacy("legacy-rule-with-body", {
				summary: "Prefer the CLI.",
				body: "Use the CLI over a browser when both work.",
				mode: "MEMORY_MODE_ALWAYS",
			});
			const playbook = legacy("legacy-playbook", {
				summary: "Read before touching ubu1's NVMe.",
				body: "Recovery steps.",
				mode: "MEMORY_MODE_ON_DEMAND",
			});
			store.kernel.exec(backfill);
			return [rule, ruleWithBody, playbook].map((id) => {
				const m = store.memories.get(id);
				return {
					content: m.content && { summary: m.content.summary, body: m.content.body },
					raw: JSON.parse(
						str(store.kernel.first("SELECT doc FROM memories WHERE id = ?", id)?.doc),
					),
				};
			});
		});
		expect(docs.map((d) => d.content)).toEqual([
			{ summary: "Bind services to 127.0.0.1.", body: "Bind services to 127.0.0.1." },
			{ summary: "Prefer the CLI.", body: "Use the CLI over a browser when both work." },
			{ summary: "Read before touching ubu1's NVMe.", body: "Recovery steps." },
		]);
		expect(docs.map((d) => "mode" in d.raw.content)).toEqual([false, false, false]);
	});
});
