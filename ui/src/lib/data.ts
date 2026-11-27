import { Code, ConnectError } from "@connectrpc/connect";
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

/** What useSession found. */
export type SessionLookup =
  | { state: "pending" }
  | { state: "found"; s: Snapshot; session: Session; listed: boolean }
  | { state: "missing" }
  | { state: "failed"; error: unknown };

/**
 * A session by id, live or ended at any age. The snapshot lists the newest
 * (`listed`); GetSession, which keeps every session, answers for the rest,
 * mostly ended ones, which no longer change. A rare live one past the
 * snapshot's cap is asked again every few seconds. Only GetSession's NotFound
 * makes a session `missing`; any other error is `failed`.
 */
export function useSession(id: string): SessionLookup {
  const { data: s } = useSnapshot();
  const listed = s?.sessions.find((x) => x.id === id);
  const older = useQuery({
    queryKey: ["session", id],
    queryFn: () => clients.sessions.getSession({ id }),
    enabled: s !== undefined && listed === undefined && id !== "",
    staleTime: (q) => (q.state.data?.session?.endedAt !== undefined ? Infinity : 0),
    refetchInterval: (q) => (q.state.data !== undefined && q.state.data.session?.endedAt === undefined ? 3000 : false),
    retry: false,
  });
  if (s === undefined) return { state: "pending" };
  if (listed) return { state: "found", s, session: listed, listed: true };
  if (older.data?.session) return { state: "found", s, session: older.data.session, listed: false };
  if (id === "") return { state: "missing" };
  if (older.isError) return ConnectError.from(older.error).code === Code.NotFound ? { state: "missing" } : { state: "failed", error: older.error };
  return { state: "pending" };
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
