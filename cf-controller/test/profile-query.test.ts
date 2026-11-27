// A finished command's profile, queried on the controller, reads as the
// worker's live one does (internal/profiler.QueryProfile, internal/flame).
import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vite-plus/test";
import {
	CommandProfileSchema,
	FoldedStackSchema,
	GetProfileRequestSchema,
	ProfileFrameSchema,
} from "../src/gen/hotbed/v1/hotbed_pb.ts";
import { queryProfile } from "../src/profile-query.ts";

const raw = create(CommandProfileSchema, {
	commandId: "cmd-prof",
	totalSamples: 8n,
	frames: [
		create(ProfileFrameSchema, { name: "do_sys_open", module: "vmlinux", frameType: "kernel" }),
		create(ProfileFrameSchema, { name: "main", module: "app", frameType: "native", sourceLine: 1 }),
		create(ProfileFrameSchema, { name: "main", module: "app", frameType: "native", sourceLine: 2 }),
		create(ProfileFrameSchema, { name: "", module: "libc.so.6", frameType: "native" }),
	],
	stacks: [
		create(FoldedStackSchema, { frames: [1, 0], samples: 5n, leafType: "kernel", comm: "app" }),
		create(FoldedStackSchema, { frames: [2, 3], samples: 3n, leafType: "native", comm: "app" }),
	],
});

function query(q: string) {
	return queryProfile(raw, create(GetProfileRequestSchema, { query: q }));
}

describe("queryProfile, as internal/profiler.QueryProfile", () => {
	it("finds kernel frames by their _[k] label", () => {
		const out = query("_[k]");
		expect(out.matchedSamples).toBe(5n);
		expect(out.stacks.map((s) => s.samples)).toEqual([5n]);
	});

	it("labels a nameless frame [unknown], whatever its module", () => {
		expect(query("[unknown]").matchedSamples).toBe(3n);
		expect(query("<unknown>").matchedSamples).toBe(0n);
	});

	it("keys its frame table by function name and type, as flame.Table does", () => {
		const out = query("");
		expect(out.frames.map((f) => [f.name, f.frameType, f.sourceLine])).toEqual([
			["main", "native", 1],
			["do_sys_open", "kernel", 0],
			["", "native", 0],
		]);
		expect(out.stacks.map((s) => s.frames)).toEqual([
			[0, 1],
			[0, 2],
		]);
	});
});
