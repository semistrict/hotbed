// The golden cases in testdata/conformance/*.json, which the Go packages they
// mirror run too (internal/settle, internal/gitops and internal/profiler's
// conformance_test.go): the worker and the CLI do in Go what the controller
// does here, and the two must agree on what they are given, byte for byte.
import { fromJson, type JsonValue } from "@bufbuild/protobuf";
import { describe, expect, it } from "vite-plus/test";
import { canonicalRemote, validateRepoURL } from "../src/gitops.ts";
import {
	CommandProfileSchema,
	GetProfileRequestSchema,
	type CommandProfile,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { frameLabel, queryProfile } from "../src/profile-query.ts";
import { Screen } from "../src/settle.ts";
import gitopsFixture from "../../testdata/conformance/gitops.json";
import profileQueryFixture from "../../testdata/conformance/profile_query.json";
import settleFixture from "../../testdata/conformance/settle.json";

describe("settle conformance, as internal/settle", () => {
	// A Text is a string, or an array of parts (each a string or
	// {repeat, count}) joined.
	type Part = string | { repeat: string; count: number };
	type Text = string | Part[];
	type WantLine = Text | { line: Text; times: number };
	interface SettleCase {
		name: string;
		input: Text;
		want: WantLine[];
	}

	const text = (t: Text): string =>
		typeof t === "string"
			? t
			: t.map((p) => (typeof p === "string" ? p : p.repeat.repeat(p.count))).join("");

	const wantLines = (want: WantLine[]): string[] =>
		want.flatMap((w) =>
			typeof w === "object" && !Array.isArray(w)
				? Array.from({ length: w.times }, () => text(w.line))
				: [text(w)],
		);

	// Settles the writes the way the controller reads stored output: decoded
	// as a stream, so a rune split between writes joins up.
	function settle(...parts: Uint8Array[]): string[] {
		const out: string[] = [];
		const screen = new Screen((line) => out.push(line));
		const decoder = new TextDecoder("utf-8");
		for (const p of parts) screen.write(decoder.decode(p, { stream: true }));
		screen.flush();
		return out;
	}

	// The size under which a case is also fed split at every byte offset.
	const SPLIT_WRITE_LIMIT = 512;

	const cases = settleFixture.cases as SettleCase[];

	it("has the cases", () => {
		expect(cases.length).toBeGreaterThan(0);
	});

	it.each(cases)("$name", (c) => {
		const bytes = new TextEncoder().encode(text(c.input));
		const want = wantLines(c.want);
		expect(settle(bytes)).toEqual(want);
		if (bytes.length >= SPLIT_WRITE_LIMIT) return;
		for (let cut = 1; cut < bytes.length; cut++) {
			expect(settle(bytes.subarray(0, cut), bytes.subarray(cut)), `split at ${cut}`).toEqual(want);
		}
	});
});

describe("gitops conformance, as internal/gitops", () => {
	const { valid, invalid, localOnly } = gitopsFixture.validate;
	const invalidArgs = expect.objectContaining({ code: "INVALID_ARGS" });

	it("has the cases", () => {
		expect(valid.length).toBeGreaterThan(0);
		expect(invalid.length).toBeGreaterThan(0);
		expect(localOnly.length).toBeGreaterThan(0);
		expect(gitopsFixture.canonical.length).toBeGreaterThan(0);
	});

	it.each(valid)("accepts %j", (u) => {
		expect(() => validateRepoURL(u)).not.toThrow();
		expect(() => validateRepoURL(u, true)).not.toThrow();
	});

	it.each(invalid)("refuses %j as invalid arguments", (u) => {
		expect(() => validateRepoURL(u)).toThrow(invalidArgs);
		expect(() => validateRepoURL(u, true)).toThrow(invalidArgs);
	});

	it.each(localOnly)("accepts %j only where local repos are allowed", (u) => {
		expect(() => validateRepoURL(u)).toThrow(invalidArgs);
		expect(() => validateRepoURL(u, true)).not.toThrow();
	});

	it.each(gitopsFixture.canonical)("names %j %j", (raw, want) => {
		expect(canonicalRemote(raw)).toBe(want);
	});
});

describe("profile query conformance, as internal/profiler.QueryProfile", () => {
	interface FrameWant {
		name: string;
		module: string;
		frameType: string;
		sourceFile: string;
		sourceLine: number;
	}
	interface FunctionWant {
		frame: string;
		selfSamples: number;
		totalSamples: number;
		selfPct: number;
		totalPct: number;
	}
	interface BucketWant {
		t: number;
		samples: number;
		topFrame: string | null;
		stackIndexes: number[];
		stackSamples: number[];
	}
	interface ProfileWant {
		totalSamples: number;
		matchedSamples: number;
		kernelSamples: number;
		userSamples: number;
		frames: FrameWant[];
		folded: string[];
		topFunctions: FunctionWant[];
		buckets: BucketWant[];
	}

	// A query's reply as the fixture's `want` states it.
	function summarize(p: CommandProfile): ProfileWant {
		const label = (frame: number) => frameLabel(p.frames[frame]);
		return {
			totalSamples: Number(p.totalSamples),
			matchedSamples: Number(p.matchedSamples),
			kernelSamples: Number(p.kernelSamples),
			userSamples: Number(p.userSamples),
			frames: p.frames.map((f) => ({
				name: f.name,
				module: f.module,
				frameType: f.frameType,
				sourceFile: f.sourceFile,
				sourceLine: f.sourceLine,
			})),
			folded: p.stacks.map(
				(st) => `${st.frames.map(label).join(";")} ${st.samples} ${st.leafType} ${st.comm}`,
			),
			topFunctions: p.topFunctions.map((fn) => ({
				frame: label(fn.frame),
				selfSamples: Number(fn.selfSamples),
				totalSamples: Number(fn.totalSamples),
				selfPct: fn.selfPct,
				totalPct: fn.totalPct,
			})),
			buckets: p.buckets.map((b) => ({
				t: Number(b.t),
				samples: Number(b.samples),
				topFrame: b.topFrame < p.frames.length ? label(b.topFrame) : null,
				stackIndexes: [...b.stackIndexes],
				stackSamples: b.stackSamples.map(Number),
			})),
		};
	}

	const profileJson = profileQueryFixture.profile as unknown as JsonValue;
	const queries = profileQueryFixture.queries as unknown as {
		name: string;
		request: JsonValue;
		want: ProfileWant;
	}[];

	it("has the queries", () => {
		expect(queries.length).toBeGreaterThan(0);
	});

	it.each(queries)("$name", (q) => {
		// Each query reads the profile afresh: it must not change it.
		const raw = fromJson(CommandProfileSchema, profileJson);
		const got = queryProfile(raw, fromJson(GetProfileRequestSchema, q.request));
		expect(summarize(got)).toEqual(q.want);
		expect(raw).toEqual(fromJson(CommandProfileSchema, profileJson));
	});
});
