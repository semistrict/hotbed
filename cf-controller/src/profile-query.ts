import { create, toBinary } from "@bufbuild/protobuf";
import {
	CommandProfileSchema,
	FoldedStackSchema,
	ProfileBucketSchema,
	ProfileFrameSchema,
	ProfileFunctionSchema,
	type CommandProfile,
	type FoldedStack,
	type GetProfileRequest,
	type ProfileBucket,
	type ProfileFrame,
	type ProfileFunction,
} from "./gen/hotbed/v1/hotbed_pb.ts";

const NO_TOP_FRAME = 0xffffffff;
const MAX_SNAPSHOT_FUNCS = 200;
const MAX_PROFILE_BYTES = 1 << 20;

/**
 * A frame as flamegraphs name it, as Go's flame.Label: a `;` would split the
 * folded stack, and a kernel frame ends in `_[k]` (what `--query _[k]` finds).
 */
function frameLabel(f: ProfileFrame): string {
	let name = f.name.replaceAll(";", ":");
	if (name === "") name = "[unknown]";
	if (f.frameType === "kernel" && !name.endsWith("_[k]")) name += "_[k]";
	return name;
}

/**
 * What makes two frames one function, as Go's flame.Table keys them, so a
 * finished command's frame table matches the worker's live one.
 */
function frameKey(f: ProfileFrame): string {
	return `${f.name}\0${f.frameType}`;
}

function compareFrames(a: number[], b: number[]): number {
	const len = Math.min(a.length, b.length);
	for (let i = 0; i < len; i++) {
		if (a[i] !== b[i]) return a[i] - b[i];
	}
	return a.length - b.length;
}

interface IndexedStack {
	rawIdx: number;
	stack: FoldedStack;
}

/**
 * Applies GetProfileRequest filters (query, frameType, comm, fromMs, toMs, limit)
 * to a persisted CommandProfile stored in controller SQLite.
 * Mirrors internal/profiler.QueryProfile, frame for frame.
 */
export function queryProfile(raw: CommandProfile, req: GetProfileRequest): CommandProfile {
	const q = req.query.trim().toLowerCase();
	const ftFilter = req.frameType.trim().toLowerCase();
	const commFilter = req.comm.trim();
	const fromMs = req.fromMs;
	const toMs = req.toMs;
	const limit = req.limit > 0 ? req.limit : 200;

	const buckets: ProfileBucket[] = [];
	let bucketSamples = 0n;
	let bucketKernel = 0n;
	let bucketUser = 0n;
	let hasBucketStacks = false;
	for (const b of raw.buckets) {
		if (b.stackIndexes.length > 0 && b.stackSamples.length > 0) {
			hasBucketStacks = true;
		}
		if (fromMs > 0n && b.t + b.durationMs <= fromMs) continue;
		if (toMs > 0n && b.t > toMs) continue;
		buckets.push(b);
		bucketSamples += b.samples;
		bucketKernel += b.kernelSamples;
		bucketUser += b.userSamples;
	}

	let candidates: IndexedStack[] = [];
	if ((fromMs > 0n || toMs > 0n) && hasBucketStacks) {
		const windowCounts = new Map<number, bigint>();
		for (const b of buckets) {
			const m = Math.min(b.stackIndexes.length, b.stackSamples.length);
			for (let i = 0; i < m; i++) {
				const idx = b.stackIndexes[i];
				const cnt = b.stackSamples[i];
				if (idx < raw.stacks.length && cnt > 0n) {
					windowCounts.set(idx, (windowCounts.get(idx) ?? 0n) + cnt);
				}
			}
		}
		for (let rawIdx = 0; rawIdx < raw.stacks.length; rawIdx++) {
			const cnt = windowCounts.get(rawIdx) ?? 0n;
			if (cnt > 0n) {
				const st = raw.stacks[rawIdx];
				candidates.push({
					rawIdx,
					stack: create(FoldedStackSchema, {
						frames: [...st.frames],
						samples: cnt,
						leafType: st.leafType,
						comm: st.comm,
					}),
				});
			}
		}
	} else if ((fromMs > 0n || toMs > 0n) && raw.buckets.length > 0 && buckets.length === 0) {
		candidates = [];
	} else {
		candidates = raw.stacks.map((stack, rawIdx) => ({ rawIdx, stack }));
	}

	const queried = raw.frames.map(
		(f) =>
			q !== "" && (frameLabel(f).toLowerCase().includes(q) || f.module.toLowerCase().includes(q)),
	);
	const typed = raw.frames.map((f) => ftFilter !== "" && f.frameType.toLowerCase() === ftFilter);
	const anyFrame = (st: FoldedStack, take: boolean[]) =>
		st.frames.some((id) => id < take.length && take[id]);

	const matchedStacks: IndexedStack[] = [];
	let matchedSamples = 0n;
	let matchedKernel = 0n;
	let matchedUser = 0n;
	const fns = new Map<number, { self: bigint; total: bigint }>();

	for (const cand of candidates) {
		const st = cand.stack;
		if (commFilter !== "" && st.comm !== commFilter) continue;
		if (ftFilter !== "" && st.leafType.toLowerCase() !== ftFilter && !anyFrame(st, typed)) continue;
		if (q !== "" && !anyFrame(st, queried) && !st.comm.toLowerCase().includes(q)) continue;

		matchedStacks.push(cand);
		matchedSamples += st.samples;
		if (st.leafType === "kernel") {
			matchedKernel += st.samples;
		} else {
			matchedUser += st.samples;
		}
		for (let i = 0; i < st.frames.length; i++) {
			const id = st.frames[i];
			let fa = fns.get(id);
			if (!fa) {
				fa = { self: 0n, total: 0n };
				fns.set(id, fa);
			}
			let seenEarlier = false;
			for (let j = 0; j < i; j++) {
				if (st.frames[j] === id) {
					seenEarlier = true;
					break;
				}
			}
			if (!seenEarlier) {
				fa.total += st.samples;
			}
			if (i === st.frames.length - 1) {
				fa.self += st.samples;
			}
		}
	}

	matchedStacks.sort((a, b) => {
		if (a.stack.samples !== b.stack.samples) return a.stack.samples > b.stack.samples ? -1 : 1;
		const fc = compareFrames(a.stack.frames, b.stack.frames);
		if (fc !== 0) return fc;
		return a.stack.comm < b.stack.comm ? -1 : a.stack.comm > b.stack.comm ? 1 : 0;
	});

	const denom = Number(matchedSamples);
	const topFns: ProfileFunction[] = [];
	for (const [id, fa] of fns.entries()) {
		topFns.push(
			create(ProfileFunctionSchema, {
				frame: id,
				selfSamples: fa.self,
				totalSamples: fa.total,
				selfPct: denom > 0 ? (100 * Number(fa.self)) / denom : 0,
				totalPct: denom > 0 ? (100 * Number(fa.total)) / denom : 0,
			}),
		);
	}
	topFns.sort((a, b) => {
		if (a.selfSamples !== b.selfSamples) return a.selfSamples > b.selfSamples ? -1 : 1;
		if (a.totalSamples !== b.totalSamples) return a.totalSamples > b.totalSamples ? -1 : 1;
		const na = raw.frames[a.frame]?.name ?? "";
		const nb = raw.frames[b.frame]?.name ?? "";
		if (na !== nb) return na < nb ? -1 : 1;
		return a.frame - b.frame;
	});
	const trimmedFns = topFns.slice(0, Math.min(topFns.length, limit, MAX_SNAPSHOT_FUNCS));

	const out = create(CommandProfileSchema, {
		commandId: raw.commandId,
		node: raw.node,
		profiler: raw.profiler,
		live: false,
		active: raw.active,
		statusNote: raw.statusNote,
		sampleHz: raw.sampleHz,
		startedAt: raw.startedAt,
		updatedAt: raw.updatedAt,
		totalSamples: raw.totalSamples,
		matchedSamples,
		kernelSamples: matchedKernel,
		userSamples: matchedUser,
		comms: [...raw.comms],
		frameTypes: [...raw.frameTypes],
	});
	if (
		(fromMs > 0n || toMs > 0n) &&
		q === "" &&
		ftFilter === "" &&
		commFilter === "" &&
		bucketSamples > 0n
	) {
		out.matchedSamples = bucketSamples;
		out.kernelSamples = bucketKernel;
		out.userSamples = bucketUser;
	}

	for (let n = Math.min(matchedStacks.length, limit); ; n = Math.floor(n / 2)) {
		keepTop(out, raw.frames, matchedStacks.slice(0, n), trimmedFns, buckets);
		if (n === 0 || toBinary(CommandProfileSchema, out).byteLength <= MAX_PROFILE_BYTES) {
			return out;
		}
	}
}

function keepTop(
	out: CommandProfile,
	from: ProfileFrame[],
	stacks: IndexedStack[],
	fns: ProfileFunction[],
	buckets: ProfileBucket[],
): void {
	const frameMap = new Map<string, number>();
	const frames: ProfileFrame[] = [];
	const intern = (f: ProfileFrame): number => {
		const k = frameKey(f);
		const existing = frameMap.get(k);
		if (existing !== undefined) return existing;
		const id = frames.length;
		frameMap.set(k, id);
		frames.push(
			create(ProfileFrameSchema, {
				name: f.name,
				module: f.module,
				frameType: f.frameType,
				sourceFile: f.sourceFile,
				sourceLine: f.sourceLine,
			}),
		);
		return id;
	};

	const oldToNew = new Map<number, number>();
	out.stacks = stacks.map((e, i) => {
		oldToNew.set(e.rawIdx, i);
		return create(FoldedStackSchema, {
			frames: e.stack.frames.map((id) => (id < from.length ? intern(from[id]) : id)),
			samples: e.stack.samples,
			leafType: e.stack.leafType,
			comm: e.stack.comm,
		});
	});
	out.topFunctions = fns.map((fn) =>
		create(ProfileFunctionSchema, {
			frame: fn.frame < from.length ? intern(from[fn.frame]) : fn.frame,
			selfSamples: fn.selfSamples,
			totalSamples: fn.totalSamples,
			selfPct: fn.selfPct,
			totalPct: fn.totalPct,
		}),
	);
	out.buckets = buckets.map((b) => {
		const nb = create(ProfileBucketSchema, {
			t: b.t,
			samples: b.samples,
			kernelSamples: b.kernelSamples,
			userSamples: b.userSamples,
			topFrame: NO_TOP_FRAME,
			durationMs: b.durationMs,
		});
		if (b.topFrame !== NO_TOP_FRAME && b.topFrame < from.length) {
			nb.topFrame = intern(from[b.topFrame]);
		}
		const m = Math.min(b.stackIndexes.length, b.stackSamples.length);
		if (m > 0 && oldToNew.size > 0) {
			const refs: Array<{ idx: number; samples: bigint }> = [];
			for (let j = 0; j < m; j++) {
				const newIdx = oldToNew.get(b.stackIndexes[j]);
				if (newIdx !== undefined && b.stackSamples[j] > 0n) {
					refs.push({ idx: newIdx, samples: b.stackSamples[j] });
				}
			}
			refs.sort((a, b) => a.idx - b.idx);
			nb.stackIndexes = refs.map((r) => r.idx);
			nb.stackSamples = refs.map((r) => r.samples);
		}
		return nb;
	});
	out.frames = frames;
}
