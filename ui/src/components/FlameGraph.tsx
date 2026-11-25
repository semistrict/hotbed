import { useMemo, useState } from "react";
import type { FoldedStack, ProfileFrame } from "../api";
import { frameLabel } from "../api/folded";

export interface FlameNode {
  name: string;
  frameType: string;
  total: number;
  self: number;
  depth: number;
  children: Map<string, FlameNode>;
}

interface PositionedFrame {
  key: string;
  path: string[];
  name: string;
  frameType: string;
  total: number;
  self: number;
  depth: number;
  xPct: number;
  wPct: number;
}

const EMPTY_PATH: string[] = [];

export function buildTree(stacks: FoldedStack[], frames: readonly ProfileFrame[]): FlameNode {
  const root: FlameNode = { name: "all", frameType: "root", total: 0, self: 0, depth: 0, children: new Map() };
  for (const st of stacks) {
    const samples = Number(st.samples);
    if (st.frames.length === 0 || samples <= 0) continue;
    root.total += samples;
    let cur = root;
    for (let i = 0; i < st.frames.length; i++) {
      const frame = frames[st.frames[i]!];
      if (!frame) continue;
      const isLeaf = i === st.frames.length - 1;
      const name = frameLabel(frame);
      let child = cur.children.get(name);
      if (!child) {
        child = { name, frameType: frame.frameType || "native", total: 0, self: 0, depth: i + 1, children: new Map() };
        cur.children.set(name, child);
      }
      child.total += samples;
      if (isLeaf) child.self += samples;
      cur = child;
    }
  }
  return root;
}

/** Resolve a root-to-node frame path against the current tree; returns null if missing or empty. */
export function findNodeByPath(root: FlameNode, path: readonly string[] | null): FlameNode | null {
  if (!path || path.length === 0) return null;
  let cur = root;
  for (const seg of path) {
    const next = cur.children.get(seg);
    if (!next) return null;
    cur = next;
  }
  return cur.total > 0 ? cur : null;
}

function layoutFrom(
  root: FlameNode,
  totalRef: number,
  basePath: readonly string[],
): { frames: PositionedFrame[]; maxDepth: number } {
  const frames: PositionedFrame[] = [];
  let maxDepth = 0;

  const walk = (node: FlameNode, xPct: number, wPct: number, relDepth: number, path: string[]) => {
    if (wPct < 0.25) return; // skip sub-pixel slivers
    maxDepth = Math.max(maxDepth, relDepth);
    frames.push({
      key: path.length === 0 ? "\0root" : path.join("\0"),
      path,
      name: node.name,
      frameType: node.frameType,
      total: node.total,
      self: node.self,
      depth: relDepth,
      xPct,
      wPct,
    });
    const sorted = Array.from(node.children.values()).sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
    let cursor = xPct;
    for (const child of sorted) {
      const cw = totalRef > 0 ? (child.total / totalRef) * 100 : 0;
      if (cw >= 0.25) {
        walk(child, cursor, cw, relDepth + 1, [...path, child.name]);
      }
      cursor += cw;
    }
  };

  walk(root, 0, 100, 0, [...basePath]);
  return { frames, maxDepth };
}

function hashHue(name: string): number {
  let h = 2166136261;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return Math.abs(h) % 100;
}

function frameFill(frame: PositionedFrame, query: string): string {
  if (query && frame.name.toLowerCase().includes(query)) {
    return "var(--warning)";
  }
  const v = hashHue(frame.name);
  if (frame.frameType === "root") return "var(--muted)";
  if (frame.frameType === "kernel") {
    return `color-mix(in oklab, var(--frost) ${42 + (v % 34)}%, var(--card))`;
  }
  if (frame.frameType === "go" || frame.frameType === "python" || frame.frameType === "v8" || frame.frameType === "ruby" || frame.frameType === "hotspot") {
    return `color-mix(in oklab, var(--series-3) ${38 + (v % 34)}%, var(--card))`;
  }
  return `color-mix(in oklab, var(--primary) ${36 + (v % 36)}%, var(--card))`;
}

export function FlameGraph({ stacks, frames: frameTable, highlight = "" }: { stacks: FoldedStack[]; frames: readonly ProfileFrame[]; highlight?: string }) {
  const tree = useMemo(() => buildTree(stacks, frameTable), [stacks, frameTable]);
  const [zoomPath, setZoomPath] = useState<string[] | null>(null);
  const [hoverKey, setHoverKey] = useState<string | null>(null);

  const zoomNode = useMemo(() => findNodeByPath(tree, zoomPath), [tree, zoomPath]);
  const activeRoot = zoomNode ?? tree;
  const activeBasePath = zoomNode && zoomPath ? zoomPath : EMPTY_PATH;
  const { frames, maxDepth } = useMemo(
    () => layoutFrom(activeRoot, activeRoot.total, activeBasePath),
    [activeRoot, activeBasePath],
  );
  const hover = useMemo(() => (hoverKey ? frames.find((f) => f.key === hoverKey) ?? null : null), [frames, hoverKey]);
  const q = highlight.trim().toLowerCase();
  const rowH = 22;
  const totalH = Math.max(120, (maxDepth + 1) * (rowH + 2) + 8);

  if (tree.total === 0) {
    return <div className="rounded-lg border border-dashed border-border-strong px-4 py-8 text-center text-[12px] text-muted-foreground">No stack samples to plot.</div>;
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-muted-foreground">
        <div className="flex flex-wrap items-center gap-3 font-mono">
          <span className="inline-flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-xs" style={{ background: "color-mix(in oklab, var(--primary) 60%, var(--card))" }} />
            native
          </span>
          <span className="inline-flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-xs" style={{ background: "color-mix(in oklab, var(--series-3) 60%, var(--card))" }} />
            go / runtime
          </span>
          <span className="inline-flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-xs" style={{ background: "color-mix(in oklab, var(--frost) 60%, var(--card))" }} />
            kernel
          </span>
        </div>
        <div className="flex items-center gap-2">
          {zoomNode && (
            <button
              type="button"
              onClick={() => setZoomPath(null)}
              title={`Reset zoom (${zoomNode.name})`}
              className="max-w-[260px] truncate rounded-md border border-border-strong bg-muted px-2 py-0.5 font-mono text-[10.5px] text-foreground hover:border-primary hover:text-primary"
            >
              Reset zoom ({zoomNode.name})
            </button>
          )}
          <span className="font-mono text-[10.5px] text-faint">click any frame to zoom</span>
        </div>
      </div>

      <div className="max-h-[480px] overflow-auto rounded-lg border border-border bg-background/60 p-2">
        <div className="relative w-full min-w-[520px]" style={{ height: `${totalH}px` }}>
          {frames.map((fr) => {
            // Flamegraph grows upward from root at bottom.
            const bottom = fr.depth * (rowH + 2);
            const pctOfAll = tree.total > 0 ? (100 * fr.total) / tree.total : 0;
            const matched = q !== "" && fr.name.toLowerCase().includes(q);
            return (
              <button
                key={fr.key}
                type="button"
                onClick={() => setZoomPath(fr.path.length === 0 ? null : fr.path)}
                onMouseEnter={() => setHoverKey(fr.key)}
                onMouseLeave={() => setHoverKey((cur) => (cur === fr.key ? null : cur))}
                onFocus={() => setHoverKey(fr.key)}
                onBlur={() => setHoverKey((cur) => (cur === fr.key ? null : cur))}
                title={`${fr.name} (${fr.total} samples, ${pctOfAll.toFixed(1)}%)`}
                style={{
                  left: `${fr.xPct}%`,
                  width: `calc(${fr.wPct}% - 1px)`,
                  bottom: `${bottom}px`,
                  height: `${rowH}px`,
                  background: frameFill(fr, q),
                }}
                className={`absolute overflow-hidden rounded-[3px] border px-1.5 text-left font-mono text-[10.5px] leading-[20px] whitespace-nowrap text-ellipsis transition-opacity hover:brightness-115 ${
                  matched ? "border-warning font-semibold text-[#1c1713]" : "border-background/50 text-foreground"
                }`}
              >
                {fr.wPct > 4.5 ? fr.name : ""}
              </button>
            );
          })}
        </div>
      </div>

      <div className="flex min-h-[22px] flex-wrap items-center justify-between gap-2 rounded-md bg-muted/50 px-2.5 py-1 font-mono text-[11px]">
        {hover ? (
          <>
            <span className="truncate text-foreground">{hover.name}</span>
            <span className="num shrink-0 text-muted-foreground">
              {hover.frameType} · total {hover.total} ({((100 * hover.total) / tree.total).toFixed(1)}%) · self {hover.self} ({((100 * hover.self) / tree.total).toFixed(1)}%)
            </span>
          </>
        ) : (
          <span className="text-faint">Hover a frame to inspect sample counts and self/total share.</span>
        )}
      </div>
    </div>
  );
}
