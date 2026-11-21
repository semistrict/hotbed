import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { api, type Node, type Snapshot } from "../api";

const SLOTS = 8;

/** The whole cluster, refreshed every second; the last answer stays while the next loads. */
export function useSnapshot() {
  return useQuery({
    queryKey: ["snapshot"],
    queryFn: () => api.snapshot(),
    refetchInterval: 1000,
    placeholderData: keepPreviousData,
  });
}

/**
 * Each node's series color, by the order nodes joined, so a node keeps its
 * color when others join, leave or are filtered out. Past eight nodes there
 * is no color: those nodes appear in tables, not as lines.
 */
export function useNodeColors(nodes: Node[] | undefined): Map<string, string> {
  return useMemo(() => {
    const m = new Map<string, string>();
    const order = [...(nodes ?? [])].sort((a, b) => a.joinedAt.localeCompare(b.joinedAt) || a.id.localeCompare(b.id));
    order.slice(0, SLOTS).forEach((n, i) => m.set(n.id, `var(--series-${i + 1})`));
    return m;
  }, [nodes]);
}

export function nodeName(s: Snapshot, id: string): string {
  return s.nodes.find((n) => n.id === id)?.hostname ?? id;
}

export function repoName(s: Snapshot, id: string): string {
  return s.repos.find((r) => r.id === id)?.name ?? id;
}
