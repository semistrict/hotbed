import type { ReactNode } from "react";

/** A page's heading, with a line under it and what sits to its right. */
export function PageTitle({ title, sub, right }: { title: string; sub?: string; right?: ReactNode }) {
  return (
    <div className="rise flex flex-wrap items-end gap-3 sm:gap-4">
      <div className="min-w-0 flex-1">
        <h1 className="display break-words text-[30px] leading-[1.05] font-semibold sm:text-[38px] lg:text-[44px]">{title}</h1>
        {sub && <p className="mt-1.5 break-words text-[12.5px] leading-relaxed text-muted-foreground sm:mt-2 sm:text-[13px]">{sub}</p>}
      </div>
      {right && <div className="min-w-0 sm:ml-auto">{right}</div>}
    </div>
  );
}
