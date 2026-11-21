import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import type { Base, Workspace } from "../../api";
import { ago, bytes } from "../../lib/format";
import { Tooltip } from "./LineChart";
import { useWidth } from "./useWidth";

export interface Lane {
  nodeId: string;
  hostname: string;
  online: boolean;
  bases: Base[];
  forks: Workspace[];
}

const LANE_H = 124;
const SOIL = 100;
const LEFT = 92;

/**
 * One repo's garden: a lane per node that keeps a warm base of it. Bases sit
 * on the soil line by generation; every workspace is a stem that sprouts from
 * the base it was forked from.
 */
export function ForkTree({ lanes, now, activeIds }: { lanes: Lane[]; now: number; activeIds: Set<string> }) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const navigate = useNavigate();
  const [hover, setHover] = useState<{ ws: Workspace; x: number } | null>(null);
  const gens = [...new Set(lanes.flatMap((l) => l.bases.map((b) => b.generation)))].sort((a, b) => a - b);
  const w = Math.max(0, width - LEFT - 16);
  const band = gens.length ? w / gens.length : w;
  const gx = (g: number) => LEFT + band * gens.indexOf(g) + band / 2;

  return (
    <div ref={ref} className="relative">
      {width > 0 && (
        <svg width={width} height={lanes.length * LANE_H} className="block overflow-visible">
          {lanes.map((lane, li) => {
            const y0 = li * LANE_H;
            return (
              <g key={lane.nodeId} transform={`translate(0 ${y0})`}>
                <text x={0} y={SOIL} dy="0.32em" className="fill-foreground font-mono text-[11px]">
                  {lane.hostname}
                </text>
                {!lane.online && (
                  <text x={0} y={SOIL + 14} className="fill-faint font-mono text-[9px]">
                    offline
                  </text>
                )}
                <line x1={LEFT - 10} x2={width - 8} y1={SOIL} y2={SOIL} stroke="var(--axis)" strokeWidth={1} />
                <line x1={LEFT - 10} x2={width - 8} y1={SOIL + 3} y2={SOIL + 3} stroke="var(--grid)" strokeWidth={1} strokeDasharray="1 5" />
                {lane.bases.map((b) => {
                  const bx = gx(b.generation);
                  const forks = lane.forks.filter((f) => f.baseId === b.id);
                  const spread = Math.min(band * 0.86, Math.max(0, forks.length - 1) * 22);
                  return (
                    <g key={b.id}>
                      {forks.map((f, i) => {
                        const lx = forks.length === 1 ? bx : bx - spread / 2 + (spread * i) / (forks.length - 1);
                        const ly = 26 + (i % 2) * 16;
                        const active = activeIds.has(f.id);
                        return (
                          <g
                            key={f.id}
                            className="cursor-pointer"
                            onPointerEnter={() => setHover({ ws: f, x: lx })}
                            onPointerLeave={() => setHover(null)}
                            onClick={() => navigate({ to: "/workspaces/$id", params: { id: f.id } })}
                          >
                            <path
                              d={`M${bx},${SOIL - 7} C${bx},${SOIL - 40} ${lx},${ly + 34} ${lx},${ly + 6}`}
                              fill="none"
                              stroke={active ? "var(--primary)" : "var(--sprout)"}
                              strokeOpacity={active ? 0.9 : 0.55}
                              strokeWidth={1.5}
                              className="grow-stem"
                            />
                            <ellipse cx={lx + 4} cy={ly + 9} rx={5} ry={2.6} transform={`rotate(-35 ${lx + 4} ${ly + 9})`} fill="var(--sprout)" opacity={0.7} />
                            <circle cx={lx} cy={ly} r={hover?.ws.id === f.id ? 7 : 5.5} fill={active ? "var(--primary)" : "var(--card)"} stroke={active ? "var(--card)" : "var(--sprout)"} strokeWidth={2} className={active ? "ember-pulse" : undefined} />
                            <circle cx={lx} cy={ly} r={13} fill="transparent" />
                          </g>
                        );
                      })}
                      <BaseMarker base={b} x={bx} />
                    </g>
                  );
                })}
              </g>
            );
          })}
        </svg>
      )}
      {hover && (
        <Tooltip x={hover.x} width={width} title={`${hover.ws.id} · ${hover.ws.agent}`}>
          <div className="font-mono text-foreground">{hover.ws.branch}</div>
          <div className="text-muted-foreground">
            forked in <span className="num font-mono font-semibold text-foreground">{hover.ws.forkMillis} ms</span> · {ago(hover.ws.createdAt, now)}
          </div>
          <div className="text-muted-foreground">
            owns <span className="num font-mono font-semibold text-foreground">{bytes(hover.ws.exclusiveBytes)}</span>, shares {bytes(hover.ws.sharedBytes)}
          </div>
        </Tooltip>
      )}
    </div>
  );
}

function BaseMarker({ base, x }: { base: Base; x: number }) {
  const r = 7;
  const c = 2 * Math.PI * r;
  return (
    <g>
      {base.state === "building" ? (
        <>
          <circle cx={x} cy={SOIL} r={r} fill="var(--card)" stroke="var(--border-strong)" strokeWidth={2} />
          <circle
            cx={x}
            cy={SOIL}
            r={r}
            fill="none"
            stroke="var(--primary)"
            strokeWidth={2}
            strokeDasharray={`${c * base.progress} ${c}`}
            transform={`rotate(-90 ${x} ${SOIL})`}
          />
        </>
      ) : (
        <rect
          x={x - r}
          y={SOIL - r}
          width={r * 2}
          height={r * 2}
          rx={3}
          fill={base.state === "warm" ? "var(--primary)" : "var(--card)"}
          stroke={base.state === "warm" ? "var(--card)" : "var(--faint)"}
          strokeWidth={2}
        />
      )}
      <text x={x} y={SOIL + 17} textAnchor="middle" className={`num font-mono text-[9.5px] ${base.state === "warm" ? "fill-foreground" : "fill-faint"}`}>
        g{base.generation}
        {base.state === "building" ? ` ${Math.round(base.progress * 100)}%` : ""}
      </text>
    </g>
  );
}
