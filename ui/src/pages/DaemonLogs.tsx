import { useNavigate, useSearch } from "@tanstack/react-router";
import type { DaemonLogLevelSlug } from "../api";
import { DaemonLogsPanel } from "../components/DaemonLogsPanel";
import { PageTitle } from "./Nodes";

export interface DaemonLogsSearch {
  source?: string;
  level?: DaemonLogLevelSlug;
  since?: "15m" | "1h" | "24h" | "7d" | "all";
}

export function DaemonLogsPage() {
  const search = useSearch({ from: "/logs" });
  const navigate = useNavigate({ from: "/logs" });

  const since = search.since === "all" ? "" : (search.since ?? "24h");

  return (
    <div className="space-y-6">
      <PageTitle
        title="Daemon logs"
        sub="Rotating WARN and ERROR slog records from the controller and every worker · CLI: hotbed daemon-logs"
      />
      <DaemonLogsPanel
        source={search.source ?? ""}
        onSourceChange={(source) =>
          navigate({
            search: (prev) => ({ ...prev, source: source || undefined }),
            replace: true,
          })
        }
        level={search.level ?? ""}
        onLevelChange={(level) =>
          navigate({
            search: (prev) => ({ ...prev, level: level || undefined }),
            replace: true,
          })
        }
        since={since}
        onSinceChange={(next) =>
          navigate({
            search: (prev) => ({ ...prev, since: next === "" ? "all" : next }),
            replace: true,
          })
        }
      />
    </div>
  );
}
