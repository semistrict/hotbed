import type { AnyRouter } from "@tanstack/react-router";

/**
 * Makes a navigation that keeps the path keep the scroll too. Such a
 * navigation only rewrites view state in the query (a tab, a filter, a
 * sample), so its resetScroll defaults to false, which also carries the saved
 * scroll position to the new history entry for Back. A navigation to another
 * page resets or restores as usual, and any navigation can still say
 * otherwise. The router has no option for this default, so this wraps its
 * commitLocation, through which every Link and navigate() passes.
 */
export function keepScrollWithinPage(router: Pick<AnyRouter, "latestLocation" | "commitLocation">) {
  const commit = router.commitLocation;
  router.commitLocation = (next) => commit({ ...next, resetScroll: next.resetScroll ?? next.pathname !== router.latestLocation.pathname });
}
