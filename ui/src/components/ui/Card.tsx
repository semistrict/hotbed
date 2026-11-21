import type { ReactNode } from "react";

export function Card({ title, sub, right, children, className = "", delay = 0 }: { title?: ReactNode; sub?: ReactNode; right?: ReactNode; children: ReactNode; className?: string; delay?: number }) {
  return (
    <section
      className={`rise relative rounded-2xl border border-border bg-card/80 p-4 shadow-[0_1px_0_0_var(--border)_inset] backdrop-blur-sm sm:p-5 ${className}`}
      style={{ animationDelay: `${delay}ms` }}
    >
      {(title || right) && (
        <header className="mb-4 flex items-start gap-3">
          <div className="min-w-0">
            {title && <h2 className="text-[13px] font-semibold tracking-tight text-foreground">{title}</h2>}
            {sub && <p className="mt-0.5 text-[11.5px] text-muted-foreground">{sub}</p>}
          </div>
          {right && <div className="ml-auto shrink-0">{right}</div>}
        </header>
      )}
      {children}
    </section>
  );
}

export function StatTile({ label, value, unit, foot, trend, tone = "default", delay = 0 }: { label: string; value: ReactNode; unit?: string; foot?: ReactNode; trend?: ReactNode; tone?: "default" | "frost" | "good" | "ember"; delay?: number }) {
  const color = tone === "frost" ? "text-frost" : tone === "good" ? "text-good-ink" : tone === "ember" ? "text-primary" : "text-foreground";
  return (
    <div className="rise rounded-xl border border-border bg-card/70 px-3.5 py-3" style={{ animationDelay: `${delay}ms` }}>
      <div className="eyebrow">{label}</div>
      <div className={`mt-1.5 flex items-baseline gap-1 ${color}`}>
        <span className="display text-[28px] leading-none font-medium">{value}</span>
        {unit && <span className="font-mono text-[11px] text-muted-foreground">{unit}</span>}
      </div>
      {trend && <div className="mt-2">{trend}</div>}
      {foot && <div className="mt-1.5 text-[11px] text-muted-foreground">{foot}</div>}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border border-dashed border-border-strong px-4 py-8 text-center text-[12px] text-muted-foreground">{children}</div>;
}
