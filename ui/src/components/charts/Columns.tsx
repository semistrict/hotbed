import { useState } from "react";
import { Tooltip } from "./LineChart";
import { niceTicks, useWidth } from "./useWidth";

export interface ColumnSeries {
  id: string;
  label: string;
  color: string;
}

interface Props {
  /** One column per entry; values stack in series order from the baseline. */
  columns: { key: string; label: string; values: number[] }[];
  series: ColumnSeries[];
  height?: number;
  format: (v: number) => string;
  /** Show every nth column label on the x axis. */
  labelEvery?: number;
  /** Mark a column as the current one (the hour in progress, say). */
  current?: string;
}

const PAD = { top: 8, right: 4, bottom: 22, left: 40 };
const GAP = 2;

/** Stacked columns: 4px rounded caps, square at the baseline, 2px surface gap between segments. */
export function Columns({ columns, series, height = 170, format, labelEvery = 1, current }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const totals = columns.map((c) => c.values.reduce((a, b) => a + b, 0));
  const ticks = niceTicks(Math.max(1, ...totals) * 1.05);
  const top = ticks[ticks.length - 1] ?? 1;
  const w = Math.max(0, width - PAD.left - PAD.right);
  const h = height - PAD.top - PAD.bottom;
  const band = columns.length ? w / columns.length : 0;
  const barW = Math.min(24, Math.max(3, band - 4));
  const y = (v: number) => PAD.top + h - (v / top) * h;
  const hovered = hover === null ? null : columns[hover];

  return (
    <div ref={ref} className="relative">
      {series.length > 1 && (
        <ul className="mb-2 flex flex-wrap gap-x-3.5 gap-y-1 text-[11px] text-muted-foreground">
          {series.map((s) => (
            <li key={s.id} className="flex items-center gap-1.5">
              <span className="inline-block size-2 rounded-[2px]" style={{ background: s.color }} />
              {s.label}
            </li>
          ))}
        </ul>
      )}
      {width > 0 && (
        <svg width={width} height={height} className="block overflow-visible" onPointerLeave={() => setHover(null)}>
          {ticks.map((v) => (
            <g key={v}>
              <line x1={PAD.left} x2={PAD.left + w} y1={y(v)} y2={y(v)} stroke="var(--grid)" strokeWidth={1} />
              <text x={PAD.left - 8} y={y(v)} dy="0.32em" textAnchor="end" className="num fill-faint font-mono text-[9.5px]">
                {format(v)}
              </text>
            </g>
          ))}
          {columns.map((c, i) => {
            const cx = PAD.left + band * i + band / 2;
            let acc = 0;
            const segs = c.values.map((v, si) => {
              const y0 = y(acc);
              acc += v;
              const y1 = y(acc);
              return { si, y0, y1, isTop: c.values.slice(si + 1).every((n) => n === 0) };
            });
            return (
              <g key={c.key} onPointerEnter={() => setHover(i)} opacity={hover === null || hover === i ? 1 : 0.55}>
                <rect x={PAD.left + band * i} y={PAD.top} width={band} height={h} fill="transparent" />
                {segs.map(({ si, y0, y1, isTop }) => {
                  const segH = y0 - y1 - (si > 0 ? GAP : 0);
                  if (segH <= 0.5) return null;
                  return (
                    <path
                      key={si}
                      d={roundedTop(cx - barW / 2, y1, barW, segH, isTop ? Math.min(4, segH, barW / 2) : 0)}
                      fill={series[si]?.color}
                    />
                  );
                })}
                {c.key === current && <circle cx={cx} cy={height - 17} r={2} fill="var(--primary)" />}
                {i % labelEvery === 0 && (
                  <text x={cx} y={height - 6} textAnchor="middle" className="num fill-faint font-mono text-[9.5px]">
                    {c.label}
                  </text>
                )}
              </g>
            );
          })}
          <line x1={PAD.left} x2={PAD.left + w} y1={y(0)} y2={y(0)} stroke="var(--axis)" strokeWidth={1} />
        </svg>
      )}
      {hovered && hover !== null && (
        <Tooltip x={PAD.left + band * hover + band / 2} width={width} title={hovered.label}>
          {series.map((s, si) => (
            <div key={s.id} className="flex items-center gap-2">
              <span className="inline-block h-[2px] w-2.5 rounded-full" style={{ background: s.color }} />
              <span className="num font-mono font-semibold text-foreground">{format(hovered.values[si] ?? 0)}</span>
              <span className="text-muted-foreground">{s.label}</span>
            </div>
          ))}
        </Tooltip>
      )}
    </div>
  );
}

function roundedTop(x: number, y: number, w: number, h: number, r: number): string {
  if (r <= 0) return `M${x},${y}h${w}v${h}h${-w}z`;
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}z`;
}
