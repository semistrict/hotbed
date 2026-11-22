import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, type ProfileBucket } from "../api";
import { hhmmss } from "../lib/format";
import { FlameGraph } from "./FlameGraph";
import { Card, Empty } from "./ui/Card";

type ProfileView = "flamegraph" | "top" | "folded";

export function ContinuousProfilePanel({ id, live }: { id: string; live: boolean }) {
  const [queryText, setQueryText] = useState("");
  const [frameType, setFrameType] = useState("");
  const [comm, setComm] = useState("");
  const [windowBucket, setWindowBucket] = useState<ProfileBucket | null>(null);
  const [view, setView] = useState<ProfileView>("flamegraph");
  const [copied, setCopied] = useState(false);

  const profileQuery = useQuery({
    queryKey: ["command-profile", id, queryText, frameType, comm, windowBucket?.t ?? 0],
    queryFn: () =>
      api.commandProfile(id, {
        query: queryText || undefined,
        frameType: frameType || undefined,
        comm: comm || undefined,
        fromMs: windowBucket ? windowBucket.t : undefined,
        toMs: windowBucket ? windowBucket.t + 5000 : undefined,
        limit: 200,
      }),
    refetchInterval: live ? 5000 : false,
  });

  const p = profileQuery.data;
  const userPct = p && p.matchedSamples > 0 ? Math.round((100 * p.userSamples) / p.matchedSamples) : 0;
  const kernelPct = p && p.matchedSamples > 0 ? 100 - userPct : 0;

  const foldedText = p?.stacks.map((s) => `${s.stack} ${s.samples}`).join("\n") ?? "";

  const copyFolded = async () => {
    if (!foldedText) return;
    try {
      await navigator.clipboard.writeText(foldedText + "\n");
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Clipboard may be blocked in non-HTTPS contexts.
    }
  };

  const downloadFolded = () => {
    if (!foldedText) return;
    const blob = new Blob([foldedText + "\n"], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `hotbed-${id}-profile.folded`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const statusBadge = p ? (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[10.5px]">
      {p.active ? (
        <span className="inline-flex items-center gap-1.5 rounded-full bg-ember-wash px-2.5 py-0.5 text-primary">
          {p.live && <span className="live-dot h-1.5 w-1.5 rounded-full bg-primary" />}
          {p.live ? "sampling" : "captured"} · {p.sampleHz} Hz
        </span>
      ) : (
        <span className="rounded-full bg-muted px-2.5 py-0.5 text-muted-foreground">inactive</span>
      )}
      {p.totalSamples > 0 && (
        <span className="num rounded-full border border-border px-2.5 py-0.5 text-muted-foreground">
          {p.matchedSamples === p.totalSamples ? `${p.totalSamples.toLocaleString()} samples` : `${p.matchedSamples.toLocaleString()} / ${p.totalSamples.toLocaleString()} samples`}
          {" · "}
          {userPct}% user / {kernelPct}% kernel
        </span>
      )}
      <button
        type="button"
        onClick={() => profileQuery.refetch()}
        className="rounded-md border border-border px-2 py-0.5 text-muted-foreground hover:border-border-strong hover:text-foreground"
      >
        Refresh
      </button>
    </div>
  ) : null;

  return (
    <Card title="Continuous Profile" sub="OpenTelemetry eBPF profiler · always on · queryable live and after exit" right={statusBadge}>
      <div className="space-y-4">
        {/* Search & filter controls */}
        <div className="flex flex-wrap items-center gap-2">
          <input
            type="search"
            value={queryText}
            onChange={(e) => setQueryText(e.target.value)}
            placeholder="Filter functions, symbols, modules..."
            aria-label="Filter profile functions"
            className="min-w-[220px] flex-1 rounded-lg border border-border bg-background/70 px-3 py-1.5 font-mono text-[11.5px] text-foreground placeholder:text-faint focus:border-primary focus:outline-none"
          />
          <div className="flex items-center rounded-lg border border-border bg-background/60 p-0.5 font-mono text-[11px]">
            {(
              [
                ["flamegraph", "Flamegraph"],
                ["top", "Top functions"],
                ["folded", "Folded stacks"],
              ] as const
            ).map(([k, label]) => (
              <button
                key={k}
                type="button"
                onClick={() => setView(k)}
                className={`rounded-md px-2.5 py-1 transition-colors ${view === k ? "bg-muted text-foreground" : "text-muted-foreground hover:text-foreground"}`}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        {/* Runtime type & process comm filter chips */}
        {p && (p.frameTypes.length > 0 || p.comms.length > 0) && (
          <div className="flex flex-wrap items-center gap-2 text-[11px]">
            {p.frameTypes.length > 0 && (
              <div className="flex flex-wrap items-center gap-1">
                <span className="font-mono text-[10px] uppercase tracking-wider text-faint">Runtime:</span>
                <FilterChip active={frameType === ""} onClick={() => setFrameType("")}>
                  all
                </FilterChip>
                {p.frameTypes.map((ft) => (
                  <FilterChip key={ft} active={frameType === ft} onClick={() => setFrameType(frameType === ft ? "" : ft)}>
                    {ft}
                  </FilterChip>
                ))}
              </div>
            )}
            {p.comms.length > 0 && (
              <div className="flex flex-wrap items-center gap-1">
                <span className="ml-2 font-mono text-[10px] uppercase tracking-wider text-faint">Process:</span>
                <FilterChip active={comm === ""} onClick={() => setComm("")}>
                  all
                </FilterChip>
                {p.comms.slice(0, 10).map((c) => (
                  <FilterChip key={c} active={comm === c} onClick={() => setComm(comm === c ? "" : c)}>
                    {c}
                  </FilterChip>
                ))}
              </div>
            )}
            {windowBucket && (
              <button
                type="button"
                onClick={() => setWindowBucket(null)}
                className="ml-auto rounded-md border border-primary/50 bg-ember-wash px-2 py-0.5 font-mono text-[10.5px] text-primary"
              >
                Window {hhmmss(windowBucket.t)} (+5s) ×
              </button>
            )}
          </div>
        )}

        {/* 5-second timeline buckets */}
        {p && p.buckets.length > 1 && (
          <div>
            <div className="mb-1 flex items-center justify-between font-mono text-[10px] text-faint">
              <span>SAMPLES OVER TIME (5s buckets · click to filter)</span>
              <span>
                {hhmmss(p.buckets[0]!.t)} — {hhmmss(p.buckets[p.buckets.length - 1]!.t)}
              </span>
            </div>
            <TimelineBuckets buckets={p.buckets} selected={windowBucket} onSelect={(b) => setWindowBucket(windowBucket?.t === b.t ? null : b)} />
          </div>
        )}

        {/* Main view */}
        {profileQuery.isLoading ? (
          <div className="py-8 text-center font-mono text-[11.5px] text-muted-foreground">Loading OpenTelemetry eBPF profile...</div>
        ) : profileQuery.error ? (
          <div className="rounded-lg border border-critical/30 bg-[color-mix(in_oklab,var(--critical)_10%,transparent)] px-3 py-2 font-mono text-[11.5px] text-critical">
            {String(profileQuery.error)}
          </div>
        ) : !p || p.totalSamples === 0 ? (
          <Empty>
            {p?.statusNote
              ? `Continuous profiler: ${p.statusNote}`
              : live
                ? "Waiting for eBPF stack samples (collected continuously at 20 Hz while processes execute on CPU)..."
                : "No continuous eBPF samples were recorded for this command."}
          </Empty>
        ) : view === "flamegraph" ? (
          <FlameGraph stacks={p.stacks} highlight={queryText} />
        ) : view === "top" ? (
          <div className="max-h-[420px] overflow-auto rounded-lg border border-border">
            <table className="w-full border-collapse font-mono text-[11.5px]">
              <thead className="sticky top-0 bg-card text-left text-[10px] uppercase tracking-wider text-muted-foreground">
                <tr className="border-b border-border">
                  <th className="px-3 py-2 text-right">Self %</th>
                  <th className="px-3 py-2 text-right">Total %</th>
                  <th className="px-3 py-2 text-right">Samples</th>
                  <th className="px-3 py-2">Runtime</th>
                  <th className="px-3 py-2">Function</th>
                  <th className="px-3 py-2">Module</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {p.topFunctions.map((fn, i) => (
                  <tr key={`${fn.name}-${i}`} className="hover:bg-muted/40">
                    <td className="num px-3 py-1.5 text-right text-primary">{fn.selfPct.toFixed(1)}%</td>
                    <td className="num px-3 py-1.5 text-right text-foreground">{fn.totalPct.toFixed(1)}%</td>
                    <td className="num px-3 py-1.5 text-right text-muted-foreground">
                      {fn.selfSamples} / {fn.totalSamples}
                    </td>
                    <td className="px-3 py-1.5">
                      <RuntimePill type={fn.frameType} />
                    </td>
                    <td className="max-w-[380px] truncate px-3 py-1.5 text-foreground" title={fn.name}>
                      {fn.name}
                    </td>
                    <td className="max-w-[160px] truncate px-3 py-1.5 text-muted-foreground" title={fn.sourceFile ? `${fn.sourceFile}:${fn.sourceLine}` : fn.module}>
                      {fn.module || "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="space-y-2">
            <div className="flex items-center justify-end gap-2">
              <button
                type="button"
                onClick={copyFolded}
                className="rounded-md border border-border px-2.5 py-1 font-mono text-[11px] text-muted-foreground hover:border-border-strong hover:text-foreground"
              >
                {copied ? "Copied" : "Copy folded"}
              </button>
              <button
                type="button"
                onClick={downloadFolded}
                className="rounded-md border border-border px-2.5 py-1 font-mono text-[11px] text-muted-foreground hover:border-border-strong hover:text-foreground"
              >
                Download .folded
              </button>
            </div>
            <pre className="max-h-[360px] overflow-auto rounded-lg border border-border bg-background/60 p-3 font-mono text-[11px] leading-relaxed text-foreground/85">
              {foldedText || "(no matching stacks)"}
            </pre>
          </div>
        )}
      </div>
    </Card>
  );
}

function FilterChip({ active, onClick, children }: { active: boolean; onClick: () => void; children: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-md border px-2 py-0.5 font-mono text-[10.5px] transition-colors ${
        active ? "border-primary bg-ember-wash text-primary" : "border-border bg-background/50 text-muted-foreground hover:border-border-strong hover:text-foreground"
      }`}
    >
      {children}
    </button>
  );
}

function RuntimePill({ type }: { type: string }) {
  const cls =
    type === "kernel"
      ? "bg-frost-wash text-frost"
      : type === "go" || type === "python" || type === "v8"
        ? "bg-[color-mix(in_oklab,var(--series-3)_15%,transparent)] text-good-ink"
        : "bg-ember-wash text-primary";
  return <span className={`inline-block rounded px-1.5 py-0.5 font-mono text-[10px] ${cls}`}>{type || "native"}</span>;
}

function TimelineBuckets({
  buckets,
  selected,
  onSelect,
}: {
  buckets: ProfileBucket[];
  selected: ProfileBucket | null;
  onSelect: (b: ProfileBucket) => void;
}) {
  const maxS = Math.max(1, ...buckets.map((b) => b.samples));
  return (
    <div className="flex h-12 items-end gap-1 rounded-lg border border-border bg-background/50 px-2 py-1.5">
      {buckets.map((b) => {
        const totalPct = Math.max(8, (b.samples / maxS) * 100);
        const kernelShare = b.samples > 0 ? (b.kernelSamples / b.samples) * 100 : 0;
        const isSel = selected?.t === b.t;
        return (
          <button
            key={b.t}
            type="button"
            onClick={() => onSelect(b)}
            title={`${hhmmss(b.t)} · ${b.samples} samples (${b.userSamples} user, ${b.kernelSamples} kernel)${b.topFunction ? ` · top: ${b.topFunction}` : ""}`}
            className={`group flex h-full flex-1 flex-col justify-end rounded-xs transition-opacity ${isSel ? "ring-1 ring-primary" : "opacity-85 hover:opacity-100"}`}
          >
            <div className="flex w-full flex-col overflow-hidden rounded-t-[2px]" style={{ height: `${totalPct}%` }}>
              {kernelShare > 0 && <div style={{ height: `${kernelShare}%`, background: "var(--frost)" }} />}
              <div className="flex-1" style={{ background: "var(--primary)" }} />
            </div>
          </button>
        );
      })}
    </div>
  );
}
