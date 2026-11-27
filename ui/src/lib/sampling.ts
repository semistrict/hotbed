import { useSyncExternalStore } from "react";
import { sampleCommand, type CommandSample, type CommandStreams, type ProcessSample } from "../api";

/**
 * A sample being taken of a command, from the click to its final chunk.
 * It lives outside any component, so leaving the command's page and coming
 * back finds it still going (or done): the worker keeps the sample either
 * way, so nothing is lost by looking away.
 */
export interface Taking {
  durationSeconds: number;
  /** 0 for every process of the command. */
  pid: number;
  status: string;
  progress: number;
  /** Set once the sample is kept: how, without its sample. */
  kept: CommandSample | null;
  /** The sample as far as it has come; the whole of it once done. */
  sample: ProcessSample | null;
  /** Why a finished sample could not be kept. */
  notKept: string;
  error: string | null;
  done: boolean;
  cancelled: boolean;
}

export interface TakeOptions {
  durationSeconds: number;
  pid?: number;
}

/** The samples being taken, one at most per command. */
export function createSamplings(sample: CommandStreams["sampleCommand"]) {
  const takings = new Map<string, Taking>();
  const aborts = new Map<string, AbortController>();
  const listeners = new Set<() => void>();

  const set = (id: string, next: Taking | null) => {
    if (next) takings.set(id, next);
    else takings.delete(id);
    for (const l of listeners) l();
  };
  const update = (id: string, ac: AbortController, patch: Partial<Taking>) => {
    const cur = takings.get(id);
    // A taking dismissed, or replaced by a newer one, takes no more news.
    if (!cur || aborts.get(id) !== ac) return;
    set(id, { ...cur, ...patch });
  };

  return {
    /** Starts a sample of command id, unless one is being taken already. */
    take(id: string, opts: TakeOptions): void {
      if (takings.get(id)?.done === false) return;
      const ac = new AbortController();
      aborts.set(id, ac);
      const pid = opts.pid ?? 0;
      set(id, {
        durationSeconds: opts.durationSeconds,
        pid,
        status: "Starting Linux activity sample…",
        progress: 4,
        kept: null,
        sample: null,
        notKept: "",
        error: null,
        done: false,
        cancelled: false,
      });
      sample(
        { id, durationSeconds: opts.durationSeconds, pid },
        (ch) => {
          const patch: Partial<Taking> = {};
          if (ch.status) patch.status = ch.status;
          if (ch.progressPct > 0) patch.progress = ch.progressPct;
          if (ch.sample) patch.sample = ch.sample;
          update(id, ac, patch);
        },
        ac.signal,
      ).then(
        (final) =>
          update(id, ac, {
            done: true,
            progress: 100,
            status: "",
            sample: final?.sample ?? takings.get(id)?.sample ?? null,
            kept: final?.kept ?? null,
            notKept: final ? final.notKept : "the sample ended without a result",
          }),
        (e: unknown) =>
          update(id, ac, {
            done: true,
            status: "",
            cancelled: ac.signal.aborted,
            error: ac.signal.aborted ? null : e instanceof Error ? e.message : String(e),
          }),
      );
    },
    cancel(id: string): void {
      aborts.get(id)?.abort();
    },
    /** Forgets a finished taking: its sample, if kept, is the command's now. */
    dismiss(id: string): void {
      if (takings.get(id)?.done === false) return;
      aborts.delete(id);
      set(id, null);
    },
    get(id: string): Taking | null {
      return takings.get(id) ?? null;
    },
    subscribe(l: () => void): () => void {
      listeners.add(l);
      return () => listeners.delete(l);
    },
  };
}

export type Samplings = ReturnType<typeof createSamplings>;

export const samplings: Samplings = createSamplings(sampleCommand);

/** The sample being taken of command id, if any. */
export function useTaking(id: string): Taking | null {
  return useSyncExternalStore(samplings.subscribe, () => samplings.get(id));
}

/** A kept sample's short name: its seq, when it was taken, and what it watched. */
export function sampleLabel(s: Pick<CommandSample, "seq" | "durationSeconds" | "pid">): string {
  return `#${s.seq} · ${s.durationSeconds}s${s.pid ? ` · pid ${s.pid}` : ""}`;
}
