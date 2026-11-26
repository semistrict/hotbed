import { type UIEvent, useCallback, useState } from "react";

// A live view follows its newest edge only while the reader is at it:
// scrolling away to read something holds the view still, and scrolling
// back (or asking to follow) resumes it.

type Scroll = { scrollTop: number; scrollHeight: number; clientHeight: number };

// The slack absorbs subpixel scroll positions on zoomed or high-DPI screens.
const SLACK = 4;

export function atEnd({ scrollTop, scrollHeight, clientHeight }: Scroll): boolean {
  return scrollHeight - scrollTop - clientHeight <= SLACK;
}

export function atStart({ scrollTop }: Pick<Scroll, "scrollTop">): boolean {
  return scrollTop <= SLACK;
}

/**
 * What a live view shows: data while the reader follows it, else held, what
 * they were reading as they stopped following; behind says newer data waits.
 */
export function heldView<T>(data: T, held: T, live: boolean, following: boolean): { shown: T; holding: boolean; behind: boolean } {
  const holding = live && !following;
  return { shown: holding ? held : data, holding, behind: holding && held !== data };
}

/**
 * Holds a live view still while its reader reads: edge is where its newest
 * data is ("end" for a log growing down, "start" for a list newest first).
 * Give the scroll box onScroll; follow() resumes following, and the caller
 * scrolls back to the edge when following turns true.
 */
export function useHeld<T>(data: T, live: boolean, edge: "start" | "end") {
  const [readingAway, setReadingAway] = useState(false);
  const [held, setHeld] = useState(data);
  const { shown, holding, behind } = heldView(data, held, live, !readingAway);
  // Derived state, set as it renders: what the reader saw last while following.
  if (!holding && held !== data) setHeld(data);
  const follow = useCallback(() => setReadingAway(false), []);
  return {
    shown,
    following: !holding,
    behind,
    onScroll: (e: UIEvent<HTMLElement>) => setReadingAway(!(edge === "end" ? atEnd(e.currentTarget) : atStart(e.currentTarget))),
    follow,
  };
}
