import { useState } from "react";
import { ChartBox } from "./ChartBox";
import { Tooltip } from "./LineChart";

interface Props {
  rows: { id: string; label: string; cells: { key: string; label: string; v: number | null }[] }[];
  format: (v: number) => string;
  /** Column labels to show, by index. */
  labelEvery?: number;
  /** What the ends of the colour ramp mean. */
  scale?: { low: string; high: string };
}

/**
 * Below this width the row labels and column ticks tighten. The legend, drawn
 * before the width is measured, follows by container query: keep its
 * `pl-[58px] @min-[420px]:pl-[78px]` in step with these three.
 */
const NARROW = 420;
const LABEL_W = 78;
const NARROW_LABEL_W = 58;
const CELL_H = 22;
const STEPS = ["var(--heat-1)", "var(--heat-2)", "var(--heat-3)", "var(--heat-4)", "var(--heat-5)"];
const LEGEND_STEPS = ["var(--heat-0)", ...STEPS];

/** Rows over time, each cell one step of a single-hue ramp; empty cells are missing data. */
export function Heatmap({ rows, format, labelEvery = 3, scale = { low: "0%", high: "100% busy" } }: Props) {
  return (
    <div className="@container min-w-0">
      <ChartBox height={rows.length * CELL_H + 20}>{(width) => <HeatmapPlot rows={rows} format={format} labelEvery={labelEvery} width={width} />}</ChartBox>
      <div className="mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 pl-[58px] text-[10px] text-faint @min-[420px]:pl-[78px]">
        <span>{scale.low}</span>
        {LEGEND_STEPS.map((s) => (
          <span key={s} className="inline-block h-2 w-4 rounded-[2px] sm:w-5" style={{ background: s }} />
        ))}
        <span>{scale.high}</span>
        <span className="ml-2 inline-block h-2 w-4 rounded-[2px] border border-dashed border-border-strong sm:ml-3 sm:w-5" />
        <span>offline</span>
      </div>
    </div>
  );
}

function HeatmapPlot({ rows, format, labelEvery, width }: Required<Pick<Props, "rows" | "format" | "labelEvery">> & { width: number }) {
  const [hover, setHover] = useState<{ r: number; c: number } | null>(null);
  const axisRow = rows.reduce<(typeof rows)[number] | undefined>(
    (best, r) => (!best || r.cells.length > best.cells.length ? r : best),
    undefined,
  );
  const cols = axisRow?.cells.length ?? 0;
  const narrow = width < NARROW;
  const labelW = narrow ? NARROW_LABEL_W : LABEL_W;
  const step = narrow ? Math.max(labelEvery, 6) : labelEvery;
  const cellW = cols ? Math.max(0, width - labelW) / cols : 0;
  const height = rows.length * CELL_H + 20;
  const cell = hover ? rows[hover.r]?.cells[hover.c] : undefined;
  const row = hover ? rows[hover.r] : undefined;

  return (
    <>
      <svg width={width} height={height} className="block" onPointerLeave={() => setHover(null)}>
        {rows.map((r, ri) => (
          <g key={r.id}>
            <text x={0} y={ri * CELL_H + CELL_H / 2} dy="0.32em" className="fill-muted-foreground font-mono text-[10px]">
              {r.label.length > (narrow ? 8 : 11) ? `${r.label.slice(0, narrow ? 7 : 10)}…` : r.label}
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
        {axisRow?.cells.map((c, ci) =>
          ci % step === 0 ? (
            <text key={c.key} x={labelW + ci * cellW + cellW / 2} y={height - 5} textAnchor="middle" className="num fill-faint font-mono text-[9.5px]">
              {c.label}
            </text>
          ) : null,
        )}
      </svg>
      {cell && row && hover && (
        <Tooltip x={labelW + hover.c * cellW + cellW / 2} width={width} title={`${row.label} · ${cell.label}`}>
          <span className="num font-mono font-semibold text-foreground">{cell.v === null ? "offline" : format(cell.v)}</span>
        </Tooltip>
      )}
    </>
  );
}
