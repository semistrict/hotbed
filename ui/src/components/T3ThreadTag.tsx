import { cn } from "@/lib/utils";
import { T3Mark } from "./T3Mark";

/**
 * A T3 Code thread: T3 Code's mark and its id's last eight characters (the
 * first may be the same "mcp:launch:" for every thread an agent launched);
 * with the link T3 Code gives the thread, choosing it opens the thread (T3
 * Code's page offers its app or the web).
 */
export function T3ThreadTag({ id, url, className = "" }: { id: string; url?: string; className?: string }) {
  const label = (
    <>
      <T3Mark />
      {id.slice(-8)}
    </>
  );
  if (!url) {
    return (
      <span className={cn("inline-flex items-center gap-1", className)} title={`T3 Code thread ${id}`}>
        {label}
      </span>
    );
  }
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer"
      className={cn("inline-flex items-center gap-1 hover:text-primary hover:underline", className)}
      title={`Open T3 Code thread ${id}`}
    >
      {label}
      <span aria-hidden="true">↗</span>
    </a>
  );
}
