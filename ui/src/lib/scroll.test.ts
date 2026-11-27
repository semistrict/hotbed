import { createMemoryHistory, createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { expect, it } from "vitest";
import { keepScrollWithinPage } from "./scroll";

it("keeps the scroll for a navigation within the page, not one to another page, unless told", async () => {
  const root = createRootRoute();
  const router = createRouter({
    routeTree: root.addChildren([createRoute({ getParentRoute: () => root, path: "/commands" }), createRoute({ getParentRoute: () => root, path: "/nodes" })]),
    history: createMemoryHistory({ initialEntries: ["/commands"] }),
    // A client router, run here without a window.
    isServer: false,
    origin: "http://localhost",
  });
  await router.load();
  // What reaches the router's own commit, under the wrapper every navigation goes through.
  const resets: (boolean | undefined)[] = [];
  const commit = router.commitLocation;
  router.commitLocation = (next) => {
    resets.push(next.resetScroll);
    return commit(next);
  };
  keepScrollWithinPage(router);

  await router.navigate({ href: "/commands?tab=failed" });
  await router.navigate({ href: "/nodes" });
  await router.navigate({ href: "/nodes?tab=x", resetScroll: true });
  await router.navigate({ href: "/commands", resetScroll: false });
  expect(resets).toEqual([false, true, true, false]);
  expect(router.state.location.pathname).toBe("/commands");
});
