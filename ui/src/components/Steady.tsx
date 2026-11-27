import { type ReactNode, useLayoutEffect, useRef } from "react";
import { lastAsk, watchAsks } from "../lib/asks";
import { FREE, steady, type Hold } from "../lib/steady";

/**
 * A box for content that refreshes on its own: it may grow, but it shrinks
 * only where nobody sees it move (lib/steady.ts has the rules). Card and
 * StatTile are steady already; use this for live content outside them.
 */
export function Steady({ children }: { children: ReactNode }) {
  const outer = useRef<HTMLDivElement>(null);
  const inner = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const o = outer.current;
    const i = inner.current;
    if (!o || !i) return;
    watchAsks();
    let hold: Hold = FREE;
    let frame = 0;
    let following = false;
    // The inner box is never held, so it always has the content's natural size.
    const look = (visible = hold.visible) => {
      const content = i.getBoundingClientRect();
      hold = steady(hold, {
        height: content.height,
        width: content.width,
        visible,
        // innerHeight, not the layout viewport: on phones it counts the strip a collapsed toolbar leaves.
        room: window.innerHeight - o.getBoundingClientRect().top,
        asked: lastAsk(),
        now: performance.now(),
      });
      o.style.minHeight = hold.held > 0 ? `${hold.held}px` : "";
      // Only a box holding space it no longer fills can give some back as the viewer scrolls.
      const slack = hold.held > hold.natural;
      if (slack !== following) {
        following = slack;
        if (slack) addEventListener("scroll", onScroll, { passive: true });
        else removeEventListener("scroll", onScroll);
      }
    };
    const onScroll = () => {
      frame ||= requestAnimationFrame(() => {
        frame = 0;
        look();
      });
    };
    const ro = new ResizeObserver(() => look());
    const io = new IntersectionObserver((entries) => {
      const entry = entries[entries.length - 1];
      if (entry) look(entry.isIntersecting);
    });
    ro.observe(i);
    io.observe(o);
    return () => {
      ro.disconnect();
      io.disconnect();
      removeEventListener("scroll", onScroll);
      cancelAnimationFrame(frame);
      o.style.minHeight = "";
    };
  }, []);
  return (
    <div ref={outer}>
      {/* flow-root keeps the content's margins inside the box it measures. */}
      <div ref={inner} className="flow-root">
        {children}
      </div>
    </div>
  );
}
