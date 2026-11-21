import { useState } from "react";
import { hhmm, hhmmss } from "../../lib/format";
import { niceTicks, useWidth } from "./useWidth";

export interface LineSeries {
  id: string;
  label: string;
  color: string;
  points: { t: number; v: number }[];
}

interface Props {
  series: LineSeries[];
  height?: number;
  /** Fixed top of the y axis, such as 1 for a fraction; otherwise from the data. */
  yMax?: number;
  format: (v: number) => string;
  /** A horizontal reference line, such as an admission threshold. */
  threshold?: { v: number; label: string };
  legend?: boolean;
}

const PAD = { top: 10, right: 14, bottom: 22, left: 40 };

/** Lines over time with a crosshair that reads every series at the pointer's time. */
export function LineChart({ series, height = 180, yMax, format, threshold, legend = true }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const all = series.flatMap((s) => s.points);
  const t0 = Math.min(...all.map((p) => p.t));
  const t1 = Math.max(...all.map((p) => p.t));
  const dataMax = Math.max(0, ...all.map((p) => p.v), threshold?.v ?? 0);
  const ticks = niceTicks(yMax ?? dataMax * 1.1);
  const top = ticks[ticks.length - 1] ?? 1;
  const w = Math.max(0, width - PAD.left - PAD.right);
  const h = height - PAD.top - PAD.bottom;
  const x = (t: number) => PAD.left + (t1 > t0 ? ((t - t0) / (t1 - t0)) * w : 0);
  const y = (v: number) => PAD.top + h - (Math.min(v, top) / top) * h;
  const times = series[0]?.points.map((p) => p.t) ?? [];
  const hoverT = hover === null ? null : times[hover];
  const xTicks = times.length ? [0, 0.25, 0.5, 0.75, 1].map((f) => t0 + f * (t1 - t0)) : [];

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    if (!times.length) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - rect.left;
    let best = 0;
    for (let i = 0; i < times.length; i++) {
      if (Math.abs(x(times[i] ?? 0) - px) < Math.abs(x(times[best] ?? 0) - px)) best = i;
    }
    setHover(best);
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
        <svg width={width} height={height} onPointerMove={onMove} onPointerLeave={() => setHover(null)} className="block touch-none overflow-visible">
          {ticks.map((v) => (
            <g key={v}>
              <line x1={PAD.left} x2={PAD.left + w} y1={y(v)} y2={y(v)} stroke="var(--grid)" strokeWidth={1} />
              <text x={PAD.left - 8} y={y(v)} dy="0.32em" textAnchor="end" className="num fill-faint font-mono text-[9.5px]">
                {format(v)}
              </text>
            </g>
          ))}
          {xTicks.map((t, i) => (
            <text key={i} x={x(t)} y={height - 6} textAnchor={i === 0 ? "start" : i === 4 ? "end" : "middle"} className="num fill-faint font-mono text-[9.5px]">
              {hhmm(t)}
            </text>
          ))}
          {threshold && (
            <g>
              <line x1={PAD.left} x2={PAD.left + w} y1={y(threshold.v)} y2={y(threshold.v)} stroke="var(--warning)" strokeOpacity={0.7} strokeWidth={1} />
              <text x={PAD.left + w} y={y(threshold.v) - 5} textAnchor="end" className="fill-muted-foreground font-mono text-[9.5px]">
                {threshold.label}
              </text>
            </g>
          )}
          {series.map((s) => (
            <path
              key={s.id}
              d={s.points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("")}
              fill="none"
              stroke={s.color}
              strokeWidth={2}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
          ))}
          {series.map((s) => {
            const last = s.points[s.points.length - 1];
            return last ? <circle key={s.id} cx={x(last.t)} cy={y(last.v)} r={4} fill={s.color} stroke="var(--card)" strokeWidth={2} /> : null;
          })}
          {hoverT !== null && hoverT !== undefined && (
            <g pointerEvents="none">
              <line x1={x(hoverT)} x2={x(hoverT)} y1={PAD.top} y2={PAD.top + h} stroke="var(--axis)" strokeWidth={1} />
              {series.map((s) => {
                const p = s.points[hover ?? 0];
                return p ? <circle key={s.id} cx={x(p.t)} cy={y(p.v)} r={4} fill={s.color} stroke="var(--card)" strokeWidth={2} /> : null;
              })}
            </g>
          )}
        </svg>
      )}
      {hoverT !== null && hoverT !== undefined && (
        <Tooltip x={x(hoverT)} width={width} title={hhmmss(hoverT)}>
          {series
            .map((s) => ({ s, v: s.points[hover ?? 0]?.v ?? 0 }))
            .sort((a, b) => b.v - a.v)
            .map(({ s, v }) => (
              <div key={s.id} className="flex items-center gap-2">
                <span className="inline-block h-[2px] w-2.5 rounded-full" style={{ background: s.color }} />
                <span className="num font-mono font-semibold text-foreground">{format(v)}</span>
                <span className="text-muted-foreground">{s.label}</span>
              </div>
            ))}
        </Tooltip>
      )}
    </div>
  );
}

export function Tooltip({ x, width, title, children }: { x: number; width: number; title: string; children: React.ReactNode }) {
  const left = x > width * 0.6 ? undefined : x + 14;
  const right = x > width * 0.6 ? width - x + 14 : undefined;
  return (
    <div
      className="pointer-events-none absolute top-6 z-10 min-w-36 rounded-lg border border-border-strong bg-card/95 px-3 py-2 text-[11px] shadow-xl backdrop-blur"
      style={{ left, right }}
    >
      <div className="mb-1 font-mono text-[10px] text-faint">{title}</div>
      <div className="space-y-0.5">{children}</div>
    </div>
  );
}
