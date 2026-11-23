import { Link } from "@tanstack/react-router";

/** A session's name, linking to it. */
export function SessionLink({ id, className = "" }: { id: string; className?: string }) {
  if (!id) return <span className="font-mono text-[11px] text-faint">—</span>;
  return (
    <Link to="/sessions/$id" params={{ id }} className={`font-mono text-[11px] text-muted-foreground hover:text-primary ${className}`}>
      {id}
    </Link>
  );
}
