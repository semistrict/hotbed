import { env, runInDurableObject } from "cloudflare:test";
import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vite-plus/test";
import type { ControllerEnv } from "../src/controller-do.ts";
import { enumFromName, enumFromWord, enumName, enumWord } from "../src/enums.ts";
import {
	BoardTaskState,
	BoardTaskStateSchema,
	CommandUpdate_What,
	CommandUpdate_WhatSchema,
	FindingDecision,
	FindingDecisionSchema,
	PromptSchema,
	QuestionOrder,
	QuestionState,
	QuestionStateSchema,
	ReviewState,
	ReviewStateSchema,
	StampSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import type { ControllerStore } from "../src/store/index.ts";
import { bindValue } from "../src/store/kernel.ts";

const inController = <T>(fn: (store: ControllerStore) => T) =>
	runInDurableObject(
		(env as unknown as ControllerEnv).CONTROLLER.getByName("default"),
		(instance) => fn((instance as unknown as { store: ControllerStore }).store),
	);

describe("SqlKernel bindings", () => {
	it("binds a bigint a Number holds exactly as that Number", () => {
		expect(bindValue(BigInt(Number.MAX_SAFE_INTEGER))).toBe(Number.MAX_SAFE_INTEGER);
		expect(bindValue(-42n)).toBe(-42);
		expect(bindValue("x")).toBe("x");
	});

	it("refuses a bigint past Number.MAX_SAFE_INTEGER instead of binding a neighbour", () => {
		expect(() => bindValue(2n ** 53n + 1n)).toThrow(
			"9007199254740993 does not fit a SQLite number exactly; store it as text",
		);
		expect(() => bindValue(-(2n ** 63n))).toThrow("does not fit a SQLite number exactly");
	});

	it("throws from exec, before SQLite sees the value", async () => {
		const res = await inController((store) => {
			try {
				store.kernel.exec("SELECT ? AS v", 2n ** 64n - 1n);
				return "bound";
			} catch (err) {
				return (err as Error).message;
			}
		});
		expect(res).toBe("18446744073709551615 does not fit a SQLite number exactly; store it as text");
	});
});

describe("proto enum strings", () => {
	it("names values as hotbed.proto does", () => {
		expect(enumName(QuestionStateSchema, QuestionState.OPEN)).toBe("QUESTION_STATE_OPEN");
		expect(enumName(ReviewStateSchema, ReviewState.IN_PROGRESS)).toBe("REVIEW_STATE_IN_PROGRESS");
		expect(enumName(QuestionStateSchema, 99)).toBe("QUESTION_STATE_UNSPECIFIED");
		expect(enumFromName(ReviewStateSchema, "REVIEW_STATE_DONE")).toBe(ReviewState.DONE);
		expect(enumFromName(ReviewStateSchema, "nonsense")).toBe(ReviewState.UNSPECIFIED);
	});

	it("words values for people, and reads them back", () => {
		expect(enumWord(ReviewStateSchema, ReviewState.IN_PROGRESS)).toBe("in progress");
		expect(enumWord(FindingDecisionSchema, FindingDecision.WONT_FIX)).toBe("wont fix");
		expect(enumWord(BoardTaskStateSchema, BoardTaskState.CANCELED)).toBe("canceled");
		expect(enumFromWord(CommandUpdate_WhatSchema, "isolation lost")).toBe(
			CommandUpdate_What.ISOLATION_LOST,
		);
		expect(enumFromWord(CommandUpdate_WhatSchema, "queued")).toBe(CommandUpdate_What.UNSPECIFIED);
	});
});

describe("ranked lists", () => {
	it("moves questions and memories through one shared ranking helper, with audit trails", async () => {
		const result = await inController((store) => {
			const by = (at: number) => create(StampSchema, { at: BigInt(at), host: "test" });
			store.projects.add("rank-proj", [], by(1));
			const qs = store.questions.add(
				"rank-proj",
				"",
				["a", "b", "c"].map((h) => create(PromptSchema, { header: h, question: h })),
				null,
				by(2),
			);
			const [a, b, c] = qs.map((q) => q.id);
			store.questions.move(c, { first: true }, by(3), () => {});
			store.questions.move(a, {}, by(4), () => {});
			store.questions.move(b, { before: c }, by(5), () => {});
			const order = store.questions
				.list({
					states: [QuestionState.OPEN],
					order: QuestionOrder.RANK,
					project: "rank-proj",
					askedBy: "",
					t3codeThreadId: "",
					limit: 0,
				})
				.questions.map((q) => q.id);
			const trail = store.questions.history(b).map((ch) => ch.what);
			return { order, ids: [a, b, c], trail };
		});
		const [a, b, c] = result.ids;
		expect(result.order).toEqual([b, c, a]);
		expect(result.trail).toEqual(["asked", "moved"]);
	});
});
