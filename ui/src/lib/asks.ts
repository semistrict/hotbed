import type { QueryClient } from "@tanstack/react-query";

/**
 * When the viewer last asked for something, for steady boxes (lib/steady.ts):
 * a change that soon after is the viewer's doing, not a refresh. Asking is a
 * click, typing, sending a form, Back or Forward, or a question answered for
 * the first time (a query's first data: the filter or tab whose answer took a
 * while). Scrolling is not asking, nor is a key press of its own, so reading
 * down a page with the keyboard never counts: the keys that act click, type
 * or send a form.
 */
let last = Number.NEGATIVE_INFINITY;

export const lastAsk = () => last;

const note = () => {
  last = performance.now();
};

let watching = false;

/** Starts noting the viewer's asks on the page; once is enough. */
export function watchAsks() {
  if (watching) return;
  watching = true;
  for (const type of ["click", "input", "change", "submit", "popstate"]) addEventListener(type, note, { capture: true, passive: true });
}

/** Notes each query's first answer as an ask. No query key may change on a refresh, or every refresh would count. */
export function watchAnswers(client: QueryClient): () => void {
  return client.getQueryCache().subscribe((e) => {
    if (e.type === "updated" && e.action.type === "success" && e.query.state.dataUpdateCount === 1) note();
  });
}
