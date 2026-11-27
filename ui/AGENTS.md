# The cockpit UI

- Build on shadcn/ui (`components.json`, style base-nova) and Base UI: add a component with the shadcn CLI into `src/components/ui/` rather than hand-roll a dropdown, dialog or menu. Pages work on phones too, with the sidebar as a drawer.
- In user-facing text, say "Cloudflare Durable Object", never a bare "DO".
- Live data never moves the page under the viewer:
  - A chart draws inside `ChartBox` (`components/charts/ChartBox.tsx`), which is its declared height before it is measured and whatever its data. A chart's height may follow how many things it shows (rows, legend entries), never their values; `ChartBox.test.tsx` holds every chart to that.
  - Anything else that refreshes sits in `Card` or `StatTile`, or in `Steady` (`components/Steady.tsx`) outside them: it may grow, but shrinks only where nobody sees it move, or right after the viewer asks for something (`lib/steady.ts`).
  - Keep the last answer while a refetch loads (`placeholderData: keepPreviousData`) rather than swap in a smaller loading state.
  - Put view state (tab, filter, selection) in the query: a navigation that keeps the path keeps the scroll (`lib/scroll.ts`), so don't pass `resetScroll`.
- Name a session with `SessionLink` (`components/SessionLink.tsx`), never a hand-made link or plain text: it opens the session at any age (the page falls back to GetSession) and its card opens the session's T3 Code thread. Don't gate it on the snapshot listing the session; `SessionLink.sources.test.ts` holds pages to both.
