import { useCallback, useEffect, useState, type DragEvent, type ReactNode } from "react";
import { dropAt, placement } from "../lib/rankedList";

/** What a list hands each item it renders. */
export interface SortableItem {
  /** The step-up/drag/step-down handle, or null when the list is not in a movable order. */
  mover: ReactNode;
  /** Tell the list the item is being edited, which holds it still. */
  onEditingChange: (id: number, editing: boolean) => void;
}

/**
 * Cards in the user's order: when `movable`, each can be dragged by its
 * handle, or stepped up and down, to a new place, unless it is being edited.
 * A drag starts only from the handle, so selecting text in a card never
 * starts one.
 */
export function SortableList<T extends { id: number }>({
  items,
  movable,
  onMove,
  render,
}: {
  items: readonly T[];
  movable: boolean;
  onMove: (id: number, before: number | null) => void;
  render: (item: T, index: number, ctx: SortableItem) => ReactNode;
}) {
  const [dragged, setDragged] = useState<number | null>(null);
  const [armedId, setArmedId] = useState<number | null>(null);
  const [editingIds, setEditingIds] = useState<Set<number>>(() => new Set());
  const [drop, setDrop] = useState<{ id: number; after: boolean } | null>(null);
  const order = items.map((x) => x.id);

  // The handle arms a drag on pointer down; letting go anywhere disarms it.
  useEffect(() => {
    if (armedId === null) return;
    const clear = () => setArmedId(null);
    window.addEventListener("pointerup", clear);
    window.addEventListener("pointercancel", clear);
    return () => {
      window.removeEventListener("pointerup", clear);
      window.removeEventListener("pointercancel", clear);
    };
  }, [armedId]);

  const onEditingChange = useCallback((id: number, isEditing: boolean) => {
    setEditingIds((prev) => {
      if (prev.has(id) === isEditing) return prev;
      const next = new Set(prev);
      if (isEditing) next.add(id);
      else next.delete(id);
      return next;
    });
    if (isEditing) setArmedId((cur) => (cur === id ? null : cur));
  }, []);

  const moveTo = (id: number, to: number) => {
    const p = placement(order, id, to);
    if (p) onMove(id, p.before);
  };
  const reset = () => {
    setDragged(null);
    setDrop(null);
    setArmedId(null);
  };
  const over = (e: DragEvent, id: number) => {
    // Not a drop target for itself: letting go where it started moves nothing.
    if (dragged === null || dragged === id) return;
    e.preventDefault();
    const box = e.currentTarget.getBoundingClientRect();
    setDrop({ id, after: e.clientY > box.top + box.height / 2 });
  };
  const dropped = (e: DragEvent) => {
    e.preventDefault();
    const to = dragged !== null && drop ? dropAt(order, dragged, drop.id, drop.after) : undefined;
    if (dragged !== null && to !== undefined) moveTo(dragged, to);
    reset();
  };

  return (
    <ol className="space-y-4">
      {items.map((item, i) => {
        const isEditing = editingIds.has(item.id);
        const canDrag = movable && !isEditing;
        return (
          <li
            key={item.id}
            draggable={canDrag && armedId === item.id}
            onDragStart={(e) => {
              if (!canDrag || armedId !== item.id) {
                e.preventDefault();
                return;
              }
              setDragged(item.id);
              e.dataTransfer.effectAllowed = "move";
            }}
            onDragEnd={reset}
            onDragOver={(e) => over(e, item.id)}
            onDrop={dropped}
            className={`relative ${dragged === item.id ? "opacity-40" : ""}`}
          >
            {drop?.id === item.id && dragged !== item.id && (
              <span aria-hidden className={`absolute right-2 left-2 h-0.5 rounded-full bg-primary ${drop.after ? "-bottom-2.5" : "-top-2.5"}`} />
            )}
            {render(item, i, {
              onEditingChange,
              mover: movable ? (
                <Mover
                  first={i === 0}
                  last={i === items.length - 1}
                  dragDisabled={isEditing}
                  onArmDrag={() => setArmedId(item.id)}
                  onUp={() => moveTo(item.id, i - 1)}
                  onDown={() => moveTo(item.id, i + 1)}
                />
              ) : null,
            })}
          </li>
        );
      })}
    </ol>
  );
}

function Mover({
  first,
  last,
  dragDisabled,
  onArmDrag,
  onUp,
  onDown,
}: {
  first: boolean;
  last: boolean;
  dragDisabled: boolean;
  onArmDrag: () => void;
  onUp: () => void;
  onDown: () => void;
}) {
  const btn = "rounded p-1.5 leading-none text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-30 disabled:hover:bg-transparent sm:px-1 sm:py-0.5";
  return (
    <div className="flex shrink-0 flex-col items-center gap-0.5 font-mono text-[11px]">
      <button type="button" className={btn} disabled={first} onClick={onUp} aria-label="Move up" title="Move up">
        ▲
      </button>
      <span
        onPointerDown={dragDisabled ? undefined : onArmDrag}
        className={`select-none text-faint ${dragDisabled ? "cursor-default opacity-30" : "cursor-grab active:cursor-grabbing"}`}
        title={dragDisabled ? "Finish editing to reorder" : "Drag to reorder"}
        aria-hidden
      >
        ⋮⋮
      </span>
      <button type="button" className={btn} disabled={last} onClick={onDown} aria-label="Move down" title="Move down">
        ▼
      </button>
    </div>
  );
}
