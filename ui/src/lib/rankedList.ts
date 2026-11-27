import { useIsMutating, useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";

/** What a polled, ranked list holds: items the user orders, each revised over time. */
export interface Revised {
  id: number;
  revision: bigint;
  updatedAt: bigint;
}

/**
 * Guard a polled list against out-of-order stale responses. Keeps the
 * highest `revision` (breaking ties by `updatedAt`) seen per ID in `seen`
 * and returns the freshest known version of each incoming item that is in
 * one of `states` (when given) and passes `keep` (when given), which is
 * checked against both the incoming and the freshest version.
 */
export function mergeLatestByRevision<T extends Revised & { state: S }, S>(
  seen: Map<number, T>,
  incoming: readonly T[],
  states?: readonly S[],
  keep?: (item: T) => boolean,
): T[] {
  const out: T[] = [];
  for (const item of incoming) {
    if (keep && !keep(item)) continue;
    const prev = seen.get(item.id);
    const fresher =
      !prev || item.revision > prev.revision || (item.revision === prev.revision && item.updatedAt >= prev.updatedAt);
    const best = fresher ? item : prev;
    if (fresher) seen.set(item.id, item);
    if (keep && !keep(best)) continue;
    if (!states || states.includes(best.state)) out.push(best);
  }
  return out;
}

/**
 * Where moving item `moved` to position `to` in the user's order puts it,
 * `to` counted among the others: just before which item, or last (null).
 * Undefined when it would stay where it is.
 */
export function placement(order: number[], moved: number, to: number): { before: number | null } | undefined {
  const others = order.filter((id) => id !== moved);
  const at = Math.max(0, Math.min(to, others.length));
  if (order.indexOf(moved) === at) return undefined;
  return { before: others[at] ?? null };
}

/** The list as it is once `moved` goes just before `before`, or last: what a move shows before the controller confirms it. */
export function reorder<T extends { id: number }>(list: T[], moved: number, before: number | null): T[] {
  const item = list.find((q) => q.id === moved);
  if (!item) return list;
  const others = list.filter((q) => q.id !== moved);
  const at = before === null ? others.length : others.findIndex((q) => q.id === before);
  if (at < 0) return list;
  return [...others.slice(0, at), item, ...others.slice(at)];
}

/**
 * Where an item dragged onto another goes, counted among the others: just
 * before the target, or just after it. Undefined for a drop on itself,
 * which moves nothing.
 */
export function dropAt(order: number[], dragged: number, target: number, after: boolean): number | undefined {
  if (dragged === target) return undefined;
  const at = order.filter((id) => id !== dragged).indexOf(target);
  return at < 0 ? undefined : at + (after ? 1 : 0);
}

/**
 * Moves an item of the ranked list cached under `key` (its items in
 * `field`), showing it in its new place at once and putting it back if the
 * controller refuses. `moving` is true while a move is on its way, when the
 * list should not refetch: one landing first would show the old order again.
 */
export function useOptimisticReorder<F extends string, R extends Record<F, { id: number }[]>>({
  key,
  field,
  mutationKey,
  move,
  invalidate,
}: {
  key: QueryKey;
  field: F;
  mutationKey: QueryKey;
  move: (id: number, before: number | null) => Promise<unknown>;
  /** What to refresh once the move settles. */
  invalidate: QueryKey;
}) {
  const qc = useQueryClient();
  const moving = useIsMutating({ mutationKey }) > 0;
  const mutation = useMutation({
    mutationKey,
    mutationFn: ({ id, before }: { id: number; before: number | null }) => move(id, before),
    onMutate: async ({ id, before }) => {
      await qc.cancelQueries({ queryKey: key });
      const was = qc.getQueryData<R>(key);
      if (was) qc.setQueryData<R>(key, { ...was, [field]: reorder(was[field], id, before) });
      return { was };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.was) qc.setQueryData(key, ctx.was);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: invalidate }),
  });
  return { ...mutation, moving };
}
