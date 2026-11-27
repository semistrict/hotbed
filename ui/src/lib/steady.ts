/**
 * How a steady box sizes itself (components/Steady.tsx). It may always grow,
 * but it shrinks only where nobody sees it move: off screen, where the
 * browser's scroll anchoring keeps the view still when a box above it
 * changes, and in the part of it below the viewport's bottom edge, which
 * nothing on screen sits below. So data refreshing inside it never pulls up
 * what the viewer is looking at, nor, at the foot of the page, the scroll
 * position.
 *
 * It re-flows at once for a new width, which moves everything anyway, and for
 * the first change within ASK_MS of the viewer asking for something
 * (lib/asks.ts): a tab swaps the content at once, a slow filter when its
 * first answer comes.
 */
export interface Hold {
  /** The least height to keep, in px; 0 keeps none. */
  held: number;
  /** The content's height when last seen. */
  natural: number;
  width: number;
  visible: boolean;
  /** The ask this box last re-flowed after; a box re-flows once per ask. */
  asked: number;
}

export const FREE: Hold = { held: 0, natural: 0, width: 0, visible: false, asked: Number.NEGATIVE_INFINITY };

/** What a steady box sees each time it looks: on a resize, on coming on or off screen, and on scrolling while it holds space. */
export interface View {
  /** The content's natural size. */
  height: number;
  width: number;
  visible: boolean;
  /** From the box's top down to the viewport's bottom edge, in px. */
  room: number;
  /** When the viewer last asked for something. */
  asked: number;
  now: number;
}

/** How soon after an ask a change counts as its answer: layout-shift scoring's hadRecentInput window. */
export const ASK_MS = 500;

export function steady(h: Hold, v: View): Hold {
  if (!v.visible) return { held: 0, natural: v.height, width: v.width, visible: false, asked: h.asked };
  const changed = v.height !== h.natural || v.width !== h.width;
  const reflow = !h.visible || v.width !== h.width || (changed && v.asked > h.asked && v.now - v.asked < ASK_MS);
  const kept = reflow ? v.height : Math.max(h.held, v.height);
  const held = Math.max(v.height, Math.min(kept, v.room));
  return { held, natural: v.height, width: v.width, visible: true, asked: reflow ? v.asked : h.asked };
}
