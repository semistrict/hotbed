import type { ReactNode } from "react";
import { Card as ShadCard, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty as ShadEmpty, EmptyDescription } from "@/components/ui/empty";
import { cn } from "@/lib/utils";

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
        "rise relative gap-4 overflow-visible rounded-2xl border border-border bg-card/80 p-4 shadow-[0_1px_0_0_var(--border)_inset] ring-0 backdrop-blur-sm sm:p-5",
        className,
      )}
      style={{ animationDelay: `${delay}ms` }}
    >
      {(title || right) && (
        <CardHeader className="p-0">
          {title && <CardTitle className="text-[13px] font-semibold tracking-tight text-foreground">{title}</CardTitle>}
          {sub && <CardDescription className="text-[11.5px] text-muted-foreground">{sub}</CardDescription>}
          {right && <CardAction>{right}</CardAction>}
        </CardHeader>
      )}
      <CardContent className="p-0">{children}</CardContent>
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
      className="rise gap-0 rounded-xl border border-border bg-card/70 px-3.5 py-3 ring-0"
      style={{ animationDelay: `${delay}ms` }}
    >
      <div className="eyebrow">{label}</div>
      <div className={cn("mt-1.5 flex items-baseline gap-1", color)}>
        <span className="display text-[28px] leading-none font-medium">{value}</span>
        {unit && <span className="font-mono text-[11px] text-muted-foreground">{unit}</span>}
      </div>
      {trend && <div className="mt-2">{trend}</div>}
      {foot && <div className="mt-1.5 text-[11px] text-muted-foreground">{foot}</div>}
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
