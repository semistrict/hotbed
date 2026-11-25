import { useState } from "react";
import { Tooltip } from "./LineChart";
import { useWidth } from "./useWidth";

interface Props {
  rows: { id: string; label: string; cells: { key: string; label: string; v: number | null }[] }[];
  format: (v: number) => string;
  /** Column labels to show, by index. */
  labelEvery?: number;
  /** What the ends of the colour ramp mean. */
  scale?: { low: string; high: string };
}

const LABEL_W = 78;
const CELL_H = 22;
const STEPS = ["var(--heat-1)", "var(--heat-2)", "var(--heat-3)", "var(--heat-4)", "var(--heat-5)"];

/** Rows over time, each cell one step of a single-hue ramp; empty cells are missing data. */
export function Heatmap({ rows, format, labelEvery = 3, scale = { low: "0%", high: "100% busy" } }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<{ r: number; c: number } | null>(null);
  const cols = rows[0]?.cells.length ?? 0;
  const labelW = width > 0 && width < 420 ? 58 : LABEL_W;
  const step = width > 0 && width < 420 ? Math.max(labelEvery, 6) : labelEvery;
  const cellW = cols ? Math.max(0, width - labelW) / cols : 0;
  const height = rows.length * CELL_H + 20;
  const cell = hover ? rows[hover.r]?.cells[hover.c] : undefined;
  const row = hover ? rows[hover.r] : undefined;

  return (
    <div ref={ref} className="relative min-w-0">
      {width > 0 && (
        <svg width={width} height={height} className="block" onPointerLeave={() => setHover(null)}>
          {rows.map((r, ri) => (
            <g key={r.id}>
              <text x={0} y={ri * CELL_H + CELL_H / 2} dy="0.32em" className="fill-muted-foreground font-mono text-[10px]">
                {r.label.length > (labelW < 70 ? 8 : 11) ? `${r.label.slice(0, labelW < 70 ? 7 : 10)}…` : r.label}
              </text>
              {r.cells.map((c, ci) => (
                <rect
                  key={c.key}
                  x={labelW + ci * cellW + 1}
                  y={ri * CELL_H + 1}
                  width={Math.max(0, cellW - 2)}
                  height={CELL_H - 2}
                  rx={3}
                  fill={c.v === null ? "transparent" : c.v < 0.02 ? "var(--heat-0)" : STEPS[Math.min(4, Math.floor(c.v * 5))]}
                  stroke={c.v === null ? "var(--grid)" : hover?.r === ri && hover.c === ci ? "var(--foreground)" : "none"}
                  strokeDasharray={c.v === null ? "2 3" : undefined}
                  onPointerEnter={() => setHover({ r: ri, c: ci })}
                />
              ))}
            </g>
          ))}
          {rows[0]?.cells.map((c, ci) =>
            ci % step === 0 ? (
              <text key={c.key} x={labelW + ci * cellW + cellW / 2} y={height - 5} textAnchor="middle" className="num fill-faint font-mono text-[9.5px]">
                {c.label}
              </text>
            ) : null,
          )}
        </svg>
      )}
      <div className="mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-[10px] text-faint sm:pl-[78px]">
        <span>{scale.low}</span>
        {STEPS.map((s) => (
          <span key={s} className="inline-block h-2 w-4 rounded-[2px] sm:w-5" style={{ background: s }} />
        ))}
        <span>{scale.high}</span>
        <span className="ml-2 inline-block h-2 w-4 rounded-[2px] border border-dashed border-border-strong sm:ml-3 sm:w-5" />
        <span>offline</span>
      </div>
      {cell && row && hover && (
        <Tooltip x={labelW + hover.c * cellW + cellW / 2} width={width} title={`${row.label} · ${cell.label}`}>
          <span className="num font-mono font-semibold text-foreground">{cell.v === null ? "offline" : format(cell.v)}</span>
        </Tooltip>
      )}
    </div>
  );
}
