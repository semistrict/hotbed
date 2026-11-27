import type { ReactNode } from "react";
import { Card as ShadCard, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty as ShadEmpty, EmptyDescription } from "@/components/ui/empty";
import { cn } from "@/lib/utils";
import { Steady } from "./Steady";

export function Card({
  title,
  sub,
  right,
  children,
  className = "",
  delay = 0,
}: {
  title?: ReactNode;
  sub?: ReactNode;
  right?: ReactNode;
  children: ReactNode;
  className?: string;
  delay?: number;
}) {
  return (
    <ShadCard
      className={cn(
        "rise relative min-w-0 gap-0 overflow-visible rounded-2xl border border-border bg-card/80 p-3.5 shadow-[0_1px_0_0_var(--border)_inset] ring-0 backdrop-blur-sm sm:p-5",
        className,
      )}
      style={{ animationDelay: `${delay}ms` }}
    >
      <Steady>
        {(title || right) && (
          <CardHeader className="mb-3.5 flex flex-col gap-2 p-0 sm:mb-4 sm:flex-row sm:items-start sm:justify-between sm:gap-4">
            <div className="min-w-0 space-y-0.5">
              {title && <CardTitle className="text-[13px] font-semibold tracking-tight text-foreground">{title}</CardTitle>}
              {sub && <CardDescription className="text-[11.5px] leading-snug text-muted-foreground">{sub}</CardDescription>}
            </div>
            {right && <div className="min-w-0 shrink-0 sm:max-w-[60%]">{right}</div>}
          </CardHeader>
        )}
        <CardContent className="min-w-0 p-0">{children}</CardContent>
      </Steady>
    </ShadCard>
  );
}

export function StatTile({
  label,
  value,
  unit,
  foot,
  trend,
  tone = "default",
  delay = 0,
}: {
  label: string;
  value: ReactNode;
  unit?: string;
  foot?: ReactNode;
  trend?: ReactNode;
  tone?: "default" | "frost" | "good" | "ember";
  delay?: number;
}) {
  const color = tone === "frost" ? "text-frost" : tone === "good" ? "text-good-ink" : tone === "ember" ? "text-primary" : "text-foreground";
  return (
    <ShadCard
      size="sm"
      className="rise min-w-0 gap-0 rounded-xl border border-border bg-card/70 px-3 py-2.5 ring-0 sm:px-3.5 sm:py-3"
      style={{ animationDelay: `${delay}ms` }}
    >
      <Steady>
        <div className="eyebrow truncate">{label}</div>
        <div className={cn("mt-1.5 flex flex-wrap items-baseline gap-x-1 gap-y-0.5", color)}>
          <span className="display num text-[22px] leading-none font-medium sm:text-[26px] md:text-[28px]">{value}</span>
          {unit && <span className="font-mono text-[10.5px] text-muted-foreground sm:text-[11px]">{unit}</span>}
        </div>
        {trend && <div className="mt-2">{trend}</div>}
        {foot && <div className="mt-1.5 break-words text-[10.5px] leading-snug text-muted-foreground sm:text-[11px]">{foot}</div>}
      </Steady>
    </ShadCard>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <ShadEmpty className="rounded-lg border border-dashed border-border-strong px-4 py-8">
      <EmptyDescription className="text-[12px] text-muted-foreground">{children}</EmptyDescription>
    </ShadEmpty>
  );
}
