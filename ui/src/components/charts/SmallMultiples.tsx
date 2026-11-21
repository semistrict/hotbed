import { useState } from "react";
import { hhmm, hhmmss } from "../../lib/format";
import type { LineSeries } from "./LineChart";
import { useWidth } from "./useWidth";

const LABEL_W = 92;
const VALUE_W = 52;
const ROW_H = 34;
const GAP = 6;

/**
 * One small area chart per series on a shared time axis and a shared scale,
 * for when the lines would tangle on one chart. A crosshair reads every row
 * at the same time.
 */
export function SmallMultiples({ series, yMax, format }: { series: LineSeries[]; yMax: number; format: (v: number) => string }) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const times = series[0]?.points.map((p) => p.t) ?? [];
  const t0 = times[0] ?? 0;
  const t1 = times[times.length - 1] ?? 1;
  const w = Math.max(0, width - LABEL_W - VALUE_W);
  const x = (t: number) => LABEL_W + (t1 > t0 ? ((t - t0) / (t1 - t0)) * w : 0);
  const height = series.length * (ROW_H + GAP) + 16;

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const px = e.clientX - e.currentTarget.getBoundingClientRect().left;
    if (px < LABEL_W || px > LABEL_W + w || !times.length) return setHover(null);
    const f = (px - LABEL_W) / Math.max(1, w);
    setHover(Math.round(f * (times.length - 1)));
  };

  return (
    <div ref={ref}>
      {width > 0 && (
        <svg width={width} height={height} className="block touch-none" onPointerMove={onMove} onPointerLeave={() => setHover(null)}>
          {series.map((s, i) => {
            const top = i * (ROW_H + GAP);
            const y = (v: number) => top + ROW_H - (Math.min(v, yMax) / yMax) * ROW_H;
            const d = s.points.map((p, j) => `${j ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("");
            const shown = hover !== null ? s.points[hover] : s.points[s.points.length - 1];
            return (
              <g key={s.id}>
                <line x1={0} x2={10} y1={top + ROW_H / 2} y2={top + ROW_H / 2} stroke={s.color} strokeWidth={2} strokeLinecap="round" />
                <text x={16} y={top + ROW_H / 2} dy="0.32em" className="fill-muted-foreground font-mono text-[10.5px]">
                  {s.label}
                </text>
                <line x1={LABEL_W} x2={LABEL_W + w} y1={top + ROW_H} y2={top + ROW_H} stroke="var(--axis)" strokeWidth={1} />
                <line x1={LABEL_W} x2={LABEL_W + w} y1={top} y2={top} stroke="var(--grid)" strokeWidth={1} />
                {d && <path d={`${d}L${x(t1)},${top + ROW_H}L${x(t0)},${top + ROW_H}z`} fill={s.color} opacity={0.14} />}
                {d && <path d={d} fill="none" stroke={s.color} strokeWidth={1.5} strokeLinejoin="round" />}
                <text x={width} y={top + ROW_H / 2} dy="0.32em" textAnchor="end" className="num fill-foreground font-mono text-[11.5px] font-semibold">
                  {shown ? format(shown.v) : "—"}
                </text>
              </g>
            );
          })}
          {hover !== null && times[hover] !== undefined && (
            <g pointerEvents="none">
              <line x1={x(times[hover] ?? 0)} x2={x(times[hover] ?? 0)} y1={0} y2={height - 16} stroke="var(--foreground)" strokeOpacity={0.35} strokeWidth={1} />
              <text x={x(times[hover] ?? 0)} y={height - 3} textAnchor="middle" className="num fill-foreground font-mono text-[9.5px]">
                {hhmmss(times[hover] ?? 0)}
              </text>
            </g>
          )}
          {hover === null &&
            [0, 0.5, 1].map((f) => (
              <text key={f} x={LABEL_W + f * w} y={height - 3} textAnchor={f === 0 ? "start" : f === 1 ? "end" : "middle"} className="num fill-faint font-mono text-[9.5px]">
                {hhmm(t0 + f * (t1 - t0))}
              </text>
            ))}
        </svg>
      )}
    </div>
  );
}
