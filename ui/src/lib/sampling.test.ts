import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { CommandSampleSchema, ProcessSampleSchema, SampleCommandChunkSchema, type CommandStreams, type SampleCommandChunk } from "../api";
import { createSamplings } from "./sampling";

/** A sampleCommand whose stream the test drives by hand. */
function controlledSampler() {
  const calls: { req: Parameters<CommandStreams["sampleCommand"]>[0]; onChunk: (ch: SampleCommandChunk) => void; signal?: AbortSignal; finish: (ch: SampleCommandChunk | null) => void; fail: (e: unknown) => void }[] = [];
  const sampleCommand: CommandStreams["sampleCommand"] = (req, onChunk, signal) =>
    new Promise((finish, fail) => {
      calls.push({ req, onChunk, signal, finish, fail });
      signal?.addEventListener("abort", () => fail(new DOMException("aborted", "AbortError")));
    });
  const call = (i: number) => {
    const c = calls[i];
    if (!c) throw new Error(`sampleCommand was called ${calls.length} times, not ${i + 1}`);
    return c;
  };
  return { calls, call, sampleCommand };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

describe("samplings", () => {
  it("tracks one sample per command from progress to its kept seq", async () => {
    const { calls, call, sampleCommand } = controlledSampler();
    const s = createSamplings(sampleCommand);
    let notified = 0;
    s.subscribe(() => notified++);

    s.take("c1", { durationSeconds: 5, pid: 42 });
    s.take("c1", { durationSeconds: 1 });
    expect(calls.map((c) => c.req)).toEqual([{ id: "c1", durationSeconds: 5, pid: 42 }]);
    expect(s.get("c1")).toMatchObject({ durationSeconds: 5, pid: 42, done: false, progress: 4 });

    call(0).onChunk(create(SampleCommandChunkSchema, { status: "polling /proc", progressPct: 60 }));
    expect(s.get("c1")).toMatchObject({ status: "polling /proc", progress: 60, done: false });

    const sample = create(ProcessSampleSchema, { commandId: "c1", hostname: "ubu1" });
    const kept = create(CommandSampleSchema, { commandId: "c1", seq: 2, durationSeconds: 5, pid: 42 });
    call(0).finish(create(SampleCommandChunkSchema, { done: true, sample, kept }));
    await settle();
    expect(s.get("c1")).toMatchObject({ done: true, progress: 100, kept, sample, notKept: "", error: null });
    expect(notified).toBe(3);

    s.dismiss("c1");
    expect(s.get("c1")).toBeNull();
  });

  it("keeps a sample that could not be kept, saying why, until dismissed", async () => {
    const { call, sampleCommand } = controlledSampler();
    const s = createSamplings(sampleCommand);
    s.take("c1", { durationSeconds: 3 });
    const sample = create(ProcessSampleSchema, { commandId: "c1" });
    call(0).finish(create(SampleCommandChunkSchema, { done: true, sample, notKept: "its command ended as it was sampled" }));
    await settle();
    expect(s.get("c1")).toMatchObject({ done: true, kept: null, sample, notKept: "its command ended as it was sampled" });
  });

  it("marks a cancelled sample cancelled rather than failed, and never dismisses one still running", async () => {
    const { call, sampleCommand } = controlledSampler();
    const s = createSamplings(sampleCommand);
    s.take("c1", { durationSeconds: 3 });
    s.dismiss("c1");
    expect(s.get("c1")).toMatchObject({ done: false });
    s.cancel("c1");
    await settle();
    expect(call(0).signal?.aborted).toBe(true);
    expect(s.get("c1")).toMatchObject({ done: true, cancelled: true, error: null });
  });

  it("reports a failed sample's error", async () => {
    const { call, sampleCommand } = controlledSampler();
    const s = createSamplings(sampleCommand);
    s.take("c1", { durationSeconds: 3 });
    call(0).fail(new Error("process sampling is not configured on this worker"));
    await settle();
    expect(s.get("c1")).toMatchObject({ done: true, cancelled: false, error: "process sampling is not configured on this worker" });
  });

  it("ignores news from a taking dismissed and replaced", async () => {
    const { call, sampleCommand } = controlledSampler();
    const s = createSamplings(sampleCommand);
    s.take("c1", { durationSeconds: 3 });
    call(0).fail(new Error("first"));
    await settle();
    s.take("c1", { durationSeconds: 10 });
    call(0).onChunk(create(SampleCommandChunkSchema, { status: "stale", progressPct: 99 }));
    expect(s.get("c1")).toMatchObject({ durationSeconds: 10, status: "Starting Linux activity sample…", done: false });
  });
});
