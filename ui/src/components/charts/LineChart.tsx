import { useState } from "react";
import { hhmm, hhmmss } from "../../lib/format";
import { niceTicks, useWidth } from "./useWidth";

export interface LinePoint {
  t: number;
  v: number | null;
}

export interface ResolvedLinePoint {
  t: number;
  v: number;
}

export interface LineSeries {
  id: string;
  label: string;
  color: string;
  points: LinePoint[];
}

export interface HoveredSeriesPoint {
  series: LineSeries;
  point: ResolvedLinePoint;
}

interface Props {
  series: LineSeries[];
  /** Optional explicit width (useful for static rendering/tests); otherwise measured from the container. */
  width?: number;
  height?: number;
  /** Fixed top of the y axis, such as 1 for a fraction; otherwise from the data. */
  yMax?: number;
  format: (v: number) => string;
  /** Horizontal reference lines, such as where admission holds and resumes. */
  thresholds?: { v: number; label: string }[];
  /** Spans of time to shade, such as when a node held new work. */
  bands?: { from: number; to: number; label: string }[];
  legend?: boolean;
}

const PAD = { top: 10, right: 14, bottom: 22, left: 40 };
const DEFAULT_MATCH_WINDOW_MS = 5_000;
const MAX_MATCH_WINDOW_MS = 30_000;

/** Splits a series's points into contiguous runs of non-null samples, breaking on every null gap. */
export function splitLineRuns(points: readonly LinePoint[]): ResolvedLinePoint[][] {
  const runs: ResolvedLinePoint[][] = [];
  let current: ResolvedLinePoint[] = [];
  for (const p of points) {
    if (p.v === null) {
      if (current.length > 0) {
        runs.push(current);
        current = [];
      }
    } else {
      current.push({ t: p.t, v: p.v });
    }
  }
  if (current.length > 0) runs.push(current);
  return runs;
}

/** Builds an SVG path string with separate M...L... subpaths across null gaps and zero-length caps for isolated points. */
export function buildLinePath(points: readonly LinePoint[], x: (t: number) => number, y: (v: number) => number): string {
  return splitLineRuns(points)
    .map((run) => {
      if (run.length === 1) {
        const p = run[0]!;
        const px = x(p.t).toFixed(1);
        const py = y(p.v).toFixed(1);
        return `M${px},${py}L${px},${py}`;
      }
      return run.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("");
    })
    .join("");
}

/** Lower-median positive spacing between consecutive timestamps, robust against occasional outage gaps. */
function typicalStep(points: readonly LinePoint[]): number | null {
  const diffs: number[] = [];
  for (let i = 1; i < points.length; i++) {
    const dt = points[i]!.t - points[i - 1]!.t;
    if (dt > 0) diffs.push(dt);
  }
  if (diffs.length === 0) return null;
  diffs.sort((a, b) => a - b);
  return diffs[Math.floor((diffs.length - 1) / 2)]!;
}

/**
 * Bounded timestamp tolerance (ms) for matching hovered points across
 * independently sampled series. Uses 0.75× the typical sampling step so two
 * nodes with the same cadence and arbitrary phase offset (up to 0.5× step plus
 * jitter) match cleanly without bridging across outages.
 */
export function matchWindowMs(series: readonly LineSeries[], target?: LineSeries): number {
  const targetStep = target ? typicalStep(target.points) : null;
  const allDiffs: number[] = [];
  for (const s of series) {
    for (let i = 1; i < s.points.length; i++) {
      const dt = s.points[i]!.t - s.points[i - 1]!.t;
      if (dt > 0) allDiffs.push(dt);
    }
  }
  allDiffs.sort((a, b) => a - b);
  const chartStep = allDiffs.length > 0 ? allDiffs[Math.floor((allDiffs.length - 1) / 2)]! : null;

  let step: number | null;
  if (targetStep !== null && target && target.points.length <= 2 && chartStep !== null && targetStep > chartStep * 2) {
    step = chartStep;
  } else {
    step = targetStep ?? chartStep;
  }
  if (step === null) return DEFAULT_MATCH_WINDOW_MS;
  return Math.min(MAX_MATCH_WINDOW_MS, step * 0.75);
}

/**
 * Resolves the sample in `points` corresponding to `targetT` within `maxGapMs`.
 *
 * - An exact timestamp match is authoritative: `v === null` explicitly marks an
 *   outage at `targetT` and returns `undefined`.
 * - Otherwise selects the nearest sample overall; if it is within `maxGapMs`
 *   and non-null, returns it. If the nearest sample is `null` or outside the
 *   window, returns `undefined` so outages are never bridged.
 */
export function resolveSeriesPoint(
  points: readonly LinePoint[],
  targetT: number,
  maxGapMs: number,
): ResolvedLinePoint | undefined {
  let nearest: LinePoint | undefined;
  let nearestDist = Infinity;

  for (const pt of points) {
    const dist = Math.abs(pt.t - targetT);
    if (dist === 0) {
      return pt.v === null ? undefined : { t: pt.t, v: pt.v };
    }
    if (dist < nearestDist) {
      nearest = pt;
      nearestDist = dist;
    } else if (dist === nearestDist && nearest && nearest.v !== null && pt.v === null) {
      nearest = pt;
    }
  }

  if (!nearest || nearestDist > maxGapMs || nearest.v === null) return undefined;
  return { t: nearest.t, v: nearest.v };
}

/** Resolves all series that have a valid (non-null) point within the sampling window of `targetT`, highest value first. */
export function resolveHoverPoints(series: readonly LineSeries[], targetT: number): HoveredSeriesPoint[] {
  const out: HoveredSeriesPoint[] = [];
  for (const s of series) {
    const point = resolveSeriesPoint(s.points, targetT, matchWindowMs(series, s));
    if (point !== undefined) out.push({ series: s, point });
  }
  return out.sort((a, b) => b.point.v - a.point.v);
}

/** Lines over time with a crosshair that reads every series at the pointer's time. */
export function LineChart({ series, width: fixedWidth, height = 180, yMax, format, thresholds = [], bands = [], legend = true }: Props) {
  const [ref, measuredWidth] = useWidth<HTMLDivElement>();
  const width = fixedWidth ?? measuredWidth;
  const [hoverT, setHoverT] = useState<number | null>(null);
  const all = series.flatMap((s) => s.points);
  const times = Array.from(new Set(all.map((p) => p.t))).sort((a, b) => a - b);
  const t0 = times[0] ?? 0;
  const t1 = times[times.length - 1] ?? 0;
  const numericValues = all.flatMap((p) => (p.v === null ? [] : [p.v]));
  const dataMax = Math.max(0, ...numericValues, ...thresholds.map((th) => th.v));
  const ticks = niceTicks(yMax ?? dataMax * 1.1);
  const top = ticks[ticks.length - 1] ?? 1;
  const maxTickChars = Math.max(0, ...ticks.map((v) => format(v).length));
  const padLeft = Math.max(PAD.left, Math.ceil(maxTickChars * 6.5 + 14));
  const w = Math.max(0, width - padLeft - PAD.right);
  const h = height - PAD.top - PAD.bottom;
  const x = (t: number) => padLeft + (t1 > t0 ? ((t - t0) / (t1 - t0)) * w : 0);
  const y = (v: number) => PAD.top + h - (Math.min(v, top) / top) * h;
  const activeHoverT = hoverT !== null && times.length > 0 && hoverT >= t0 && hoverT <= t1 ? hoverT : null;
  const hovered = activeHoverT !== null ? resolveHoverPoints(series, activeHoverT) : [];
  const xTicks =
    times.length === 0 ? [] : t1 > t0 ? [0, 0.25, 0.5, 0.75, 1].map((f) => t0 + f * (t1 - t0)) : [t0];

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    if (!times.length) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - rect.left;
    let best = times[0]!;
    let bestDist = Math.abs(x(best) - px);
    for (let i = 1; i < times.length; i++) {
      const t = times[i]!;
      const dist = Math.abs(x(t) - px);
      if (dist < bestDist) {
        best = t;
        bestDist = dist;
      }
    }
    setHoverT(best);
  };

  return (
    <div ref={ref} className="relative">
      {legend && series.length > 1 && (
        <ul className="mb-2 flex flex-wrap gap-x-3.5 gap-y-1 text-[11px] text-muted-foreground">
          {series.map((s) => (
            <li key={s.id} className="flex items-center gap-1.5">
              <span className="inline-block h-[2px] w-3 rounded-full" style={{ background: s.color }} />
              {s.label}
            </li>
          ))}
        </ul>
      )}
      {width > 0 && (
        <svg width={width} height={height} onPointerMove={onMove} onPointerLeave={() => setHoverT(null)} className="block touch-none overflow-visible">
          {ticks.map((v) => (
            <g key={v}>
              <line x1={padLeft} x2={padLeft + w} y1={y(v)} y2={y(v)} stroke="var(--grid)" strokeWidth={1} />
              <text x={padLeft - 8} y={y(v)} dy="0.32em" textAnchor="end" className="num fill-faint font-mono text-[9.5px]">
                {format(v)}
              </text>
            </g>
          ))}
          {xTicks.map((t, i) => (
            <text
              key={i}
              x={x(t)}
              y={height - 6}
              textAnchor={xTicks.length === 1 ? "start" : i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}
              className="num fill-faint font-mono text-[9.5px]"
            >
              {hhmm(t)}
            </text>
          ))}
          {bands.map((b) => (
            <rect key={b.from} x={x(b.from)} y={PAD.top} width={Math.max(1, x(b.to) - x(b.from))} height={h} fill="var(--serious)" opacity={0.1}>
              <title>{b.label}</title>
            </rect>
          ))}
          {thresholds.map((th, i) => (
            <g key={th.label}>
              <line
                x1={padLeft}
                x2={padLeft + w}
                y1={y(th.v)}
                y2={y(th.v)}
                stroke="var(--warning)"
                strokeOpacity={0.7}
                strokeWidth={1}
                strokeDasharray={i > 0 ? "4 3" : undefined}
              />
              <text x={padLeft + w} y={y(th.v) - 5} textAnchor="end" className="fill-muted-foreground font-mono text-[9.5px]">
                {th.label}
              </text>
            </g>
          ))}
          {series.map((s) => {
            const d = buildLinePath(s.points, x, y);
            const last = s.points[s.points.length - 1];
            const isolated = splitLineRuns(s.points)
              .filter((run) => run.length === 1)
              .map((run) => run[0]!)
              .filter((p) => !(last && last.v !== null && last.t === p.t));
            return (
              <g key={s.id}>
                {d && (
                  <path
                    d={d}
                    fill="none"
                    stroke={s.color}
                    strokeWidth={2}
                    strokeLinejoin="round"
                    strokeLinecap="round"
                  />
                )}
                {isolated.map((p) => (
                  <circle key={p.t} cx={x(p.t)} cy={y(p.v)} r={2.5} fill={s.color} />
                ))}
              </g>
            );
          })}
          {series.map((s) => {
            const last = s.points[s.points.length - 1];
            return last && last.v !== null ? (
              <circle key={s.id} cx={x(last.t)} cy={y(last.v)} r={4} fill={s.color} stroke="var(--card)" strokeWidth={2} />
            ) : null;
          })}
          {activeHoverT !== null && (
            <g pointerEvents="none">
              <line x1={x(activeHoverT)} x2={x(activeHoverT)} y1={PAD.top} y2={PAD.top + h} stroke="var(--axis)" strokeWidth={1} />
              {hovered.map(({ series: s, point: p }) => (
                <circle key={s.id} cx={x(p.t)} cy={y(p.v)} r={4} fill={s.color} stroke="var(--card)" strokeWidth={2} />
              ))}
            </g>
          )}
        </svg>
      )}
      {activeHoverT !== null && hovered.length > 0 && (
        <Tooltip x={x(activeHoverT)} width={width} title={hhmmss(activeHoverT)}>
          {hovered.map(({ series: s, point: p }) => (
            <div key={s.id} className="flex items-center gap-2">
              <span className="inline-block h-[2px] w-2.5 rounded-full" style={{ background: s.color }} />
              <span className="num font-mono font-semibold text-foreground">{format(p.v)}</span>
              <span className="text-muted-foreground">{s.label}</span>
            </div>
          ))}
        </Tooltip>
      )}
    </div>
  );
}

export function Tooltip({ x, width, title, children }: { x: number; width: number; title: string; children: React.ReactNode }) {
  const clampedX = Math.max(0, Math.min(width, x));
  const flip = clampedX > width * 0.55;
  const maxOffset = Math.max(0, width - 160);
  const left = flip ? undefined : Math.max(0, Math.min(maxOffset, clampedX + 14));
  const right = flip ? Math.max(0, Math.min(maxOffset, width - clampedX + 14)) : undefined;
  return (
    <div
      className="pointer-events-none absolute top-6 z-10 max-w-[calc(100%-8px)] min-w-36 rounded-lg border border-border-strong bg-card/95 px-3 py-2 text-[11px] shadow-xl backdrop-blur"
      style={{ left, right }}
    >
      <div className="mb-1 font-mono text-[10px] text-faint">{title}</div>
      <div className="space-y-0.5">{children}</div>
    </div>
  );
}
