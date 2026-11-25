import { useState } from "react";

export interface Part {
  id: string;
  label: string;
  value: number;
  color: string;
  /** Optional text colour for inside labels on tall bars; defaults by contrast against `color`. */
  ink?: string;
  /** Hatched, for parts that are not quite there: swap, say. */
  hatch?: boolean;
}

function segmentInk(part: Part): string {
  if (part.ink !== undefined) return part.ink;
  if (part.color === "var(--muted)") return "var(--muted-foreground)";
  if (
    part.color === "var(--sprout)" ||
    part.color === "var(--frost)" ||
    part.color === "var(--series-4)" ||
    part.color === "var(--warning)"
  ) {
    return "#1c1713";
  }
  return "#fffdf9";
}

/** One whole split into parts, with no baseline. Thin (8px) or tall (28px, labelled). */
export function SegmentedBar({ parts, tall = false, format, legend = true }: { parts: Part[]; tall?: boolean; format: (v: number) => string; legend?: boolean }) {
  const [hover, setHover] = useState<string | null>(null);
  const total = parts.reduce((n, p) => n + (Number.isFinite(p.value) && p.value > 0 ? p.value : 0), 0) || 1;
  const shown = parts.filter((p) => Number.isFinite(p.value) && p.value > 0);
  return (
    <div>
      <div className={`flex w-full gap-[2px] ${tall ? "h-7" : "h-2"}`} role="img" aria-label={parts.map((p) => `${p.label} ${format(p.value)}`).join(", ")}>
        {shown.length === 0 ? (
          <div className={`w-full bg-muted/50 ${tall ? "rounded-[4px]" : "rounded-full"}`} />
        ) : (
          shown.map((p) => {
            const share = p.value / total;
            const ink = segmentInk(p);
            return (
              <div
                key={p.id}
                onPointerEnter={() => setHover(p.id)}
                onPointerLeave={() => setHover(null)}
                title={`${p.label}: ${format(p.value)} (${Math.round(share * 100)}%)`}
                className={`relative flex items-center overflow-hidden ${tall ? "rounded-[4px]" : "rounded-full"} transition-opacity`}
                style={{
                  width: `${share * 100}%`,
                  background: p.hatch ? `repeating-linear-gradient(45deg, ${p.color} 0 3px, transparent 3px 6px)` : p.color,
                  opacity: hover && hover !== p.id ? 0.5 : 1,
                }}
              >
                {tall && share > 0.09 && ink && (
                  <span className="num px-2 font-mono text-[10px] font-semibold mix-blend-normal" style={{ color: ink }}>
                    {Math.round(share * 100)}%
                  </span>
                )}
              </div>
            );
          })
        )}
      </div>
      {legend && (
        <ul className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
          {parts.map((p) => (
            <li key={p.id} className="flex items-center gap-1.5">
              <span
                className="inline-block size-2 rounded-[2px]"
                style={{ background: p.hatch ? `repeating-linear-gradient(45deg, ${p.color} 0 2px, transparent 2px 4px)` : p.color }}
              />
              {p.label}
              <span className="num font-mono text-foreground">{format(p.value)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** A fill against a lighter track of the same hue; turns warning, then critical, past thresholds. */
export function Meter({ value, warn = 0.8, danger = 0.92, marker }: { value: number; warn?: number; danger?: number; marker?: number }) {
  const v = Number.isFinite(value) ? Math.max(0, Math.min(1, value)) : 0;
  const safeMarker = marker !== undefined && Number.isFinite(marker) ? Math.max(0, Math.min(1, marker)) : undefined;
  const color = v >= danger ? "var(--critical)" : v >= warn ? "var(--warning)" : "var(--primary)";
  return (
    <div className="relative h-1.5 w-full rounded-full" style={{ background: `color-mix(in oklab, ${color} 18%, transparent)` }}>
      <div className="h-full rounded-full transition-[width] duration-700" style={{ width: `${v * 100}%`, background: color }} />
      {safeMarker !== undefined && (
        <div className="absolute -top-0.5 h-2.5 w-[2px] rounded bg-foreground/60" style={{ left: `${safeMarker * 100}%` }} title="guaranteed (memory.low)" />
      )}
    </div>
  );
}

/** A small line with a wash under it, for a trend beside a figure. */
export function Sparkline({ values, color = "var(--primary)", height = 28, max }: { values: number[]; color?: string; height?: number; max?: number }) {
  if (values.length === 0) {
    return <div className="w-full" style={{ height }} aria-hidden />;
  }
  const w = 120;
  const top = max ?? Math.max(1e-9, ...values);
  const pts = values.map((v, i) => [(i / Math.max(1, values.length - 1)) * w, height - 2 - (Math.min(v, top) / top) * (height - 4)] as const);
  const d = pts.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`).join("");
  return (
    <svg viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="block w-full overflow-visible" style={{ height }}>
      <path d={`${d}L${w},${height}L0,${height}z`} fill={color} opacity={0.1} />
      <path d={d} fill="none" stroke={color} strokeWidth={1.5} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
    </svg>
  );
}
