import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { clients, type Node, type Session, type Snapshot } from "../api";
import { cmpBig } from "./format";

const SLOTS = 8;

/** The whole cluster, refreshed every second; the last answer stays while the next loads. */
export function useSnapshot() {
  return useQuery({
    queryKey: ["snapshot"],
    queryFn: () => clients.cluster.getSnapshot({}),
    refetchInterval: 1000,
    placeholderData: keepPreviousData,
  });
}

/**
 * A session by id, live or ended at any age: the snapshot lists those live or
 * ended in the last day, and GetSession, which keeps every session, answers
 * for an older one. `pending` while either is still loading; then `session`
 * is undefined only for an id no session has.
 */
export function useSession(id: string): { s: Snapshot | undefined; session: Session | undefined; pending: boolean } {
  const { data: s } = useSnapshot();
  const listed = s?.sessions.find((x) => x.id === id);
  const older = useQuery({
    queryKey: ["session", id],
    queryFn: () => clients.sessions.getSession({ id }),
    enabled: s !== undefined && listed === undefined && id !== "",
    // Only an ended session falls out of the snapshot, and an ended session no longer changes.
    staleTime: Infinity,
    retry: false,
  });
  const session = listed ?? older.data?.session;
  return { s, session, pending: s === undefined || (listed === undefined && id !== "" && older.isPending) };
}

/** Retire an offline node so it leaves the cluster and cockpit while keeping its history. */
export function useRetireNode() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (node: string) => clients.cluster.retireNode({ node }),
    onSuccess: async (retired) => {
      qc.setQueryData<Snapshot>(["snapshot"], (prev) =>
        prev
          ? {
              ...prev,
              nodes: prev.nodes.filter((n) => n.id !== retired.id && n.hostname !== retired.hostname),
            }
          : prev,
      );
      await qc.invalidateQueries({ queryKey: ["snapshot"] });
    },
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
    const order = [...(nodes ?? [])].sort((a, b) => cmpBig(a.joinedAt, b.joinedAt) || a.id.localeCompare(b.id));
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
