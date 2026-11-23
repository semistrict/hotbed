import { useNavigate } from "@tanstack/react-router";
import { useProject } from "../lib/project";

/** A project's name; choosing it shows the cockpit as that project sees it. */
export function ProjectTag({ name, to }: { name: string | null | undefined; to?: "/sessions" | "/questions" }) {
  const [, choose] = useProject();
  const navigate = useNavigate();
  if (!name) return <span className="font-mono text-[11px] text-faint">—</span>;
  return (
    <button
      type="button"
      onClick={() => {
        choose(name);
        if (to) void navigate({ to });
      }}
      title={`Show only ${name}`}
      className="rounded border border-border-strong px-1.5 py-0.5 font-mono text-[10.5px] text-muted-foreground hover:border-primary/50 hover:text-primary"
    >
      ◆ {name}
    </button>
  );
}
