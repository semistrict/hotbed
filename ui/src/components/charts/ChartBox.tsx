import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

/**
 * The box a chart draws in. It is exactly `height` tall from its first layout:
 * before its width is measured, with no data yet, and on every refresh, so a
 * chart never moves what is around it. It draws once its width is known; a
 * fixed `width` (static rendering, tests) skips the wait.
 */
export function ChartBox({ height, width, children }: { height: number; width?: number; children: (width: number) => ReactNode }) {
  const [ref, measured] = useWidth();
  const w = width ?? measured;
  return (
    <div ref={ref} className="relative" style={{ height }}>
      {w > 0 && children(w)}
    </div>
  );
}

/** The rendered width of the element the ref is on, kept current on resize. */
function useWidth(): [RefObject<HTMLDivElement | null>, number] {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.getBoundingClientRect().width);
    const ro = new ResizeObserver(([entry]) => {
      if (entry) setWidth(entry.contentRect.width);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}
