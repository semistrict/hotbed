import { Server, Zap } from "lucide-react";
import { useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { NodeSystemUpgrade } from "../api";
import { useUpgradeSystem } from "../lib/settings";
import { Card } from "./Panel";

/**
 * Triggers a host system upgrade (cladefsd, firecracker, guest kernel) on one
 * node, or a rolling one across the organization's online nodes, and lists
 * what each node did.
 */
export function SystemUpgradePanel({
  node,
  orgLabel,
  canEdit,
  pendingEdits,
}: {
  /** The node to upgrade; "" rolls through every online node. */
  node: string;
  orgLabel: string;
  canEdit: boolean;
  /** Unsaved config edits, which an upgrade would not see yet. */
  pendingEdits: boolean;
}) {
  const upgradeSystem = useUpgradeSystem();
  const [results, setResults] = useState<NodeSystemUpgrade[] | null>(null);

  async function trigger() {
    setResults(null);
    try {
      const res = await upgradeSystem.mutateAsync(node);
      setResults(res.nodes);
    } catch {
      // Shown from upgradeSystem.error.
    }
  }

  return (
    <Card
      title="Run Host System Upgrade"
      sub={`Applies staged cladefsd, firecracker, and guest kernel updates for ${orgLabel} nodes one at a time. Drains active commands and releases workspaces on each target node.`}
    >
      <div className="flex flex-wrap items-center gap-2.5">
        {canEdit ? (
          <>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={upgradeSystem.isPending || pendingEdits}
              onClick={() => void trigger()}
              className="gap-1.5 font-mono text-[11.5px]"
            >
              <Zap className="size-3.5 text-primary" />
              {upgradeSystem.isPending ? "Upgrading…" : node ? `Upgrade ${node}` : "Rolling upgrade all online org nodes"}
            </Button>
            {pendingEdits && (
              <span className="font-mono text-[11px] text-muted-foreground">
                Save pending artifact changes before triggering an upgrade.
              </span>
            )}
          </>
        ) : (
          <span className="font-mono text-[11.5px] text-muted-foreground">
            Only organization admins can trigger host system upgrades.
          </span>
        )}
      </div>

      {upgradeSystem.error && (
        <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
          <AlertDescription className="text-[11px] text-critical">{String(upgradeSystem.error)}</AlertDescription>
        </Alert>
      )}

      {results && results.length > 0 && (
        <div className="mt-4 space-y-1.5">
          {results.map((r) => (
            <div
              key={r.node}
              className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border/80 bg-background/40 px-3 py-2 font-mono text-[11.5px]"
            >
              <div className="flex items-center gap-2">
                <Server className="size-3.5 text-muted-foreground" />
                <span className="font-medium text-foreground">{r.node}</span>
                <Badge
                  variant="secondary"
                  className={
                    r.error ? "bg-critical/15 text-critical" : r.status === "upgraded" ? "bg-ember-wash text-primary" : "text-muted-foreground"
                  }
                >
                  {r.status}
                </Badge>
              </div>
              <div className="flex flex-wrap items-center gap-2 text-[10.5px] text-muted-foreground">
                {r.cladefsdVersion && <span>cladefsd {r.cladefsdVersion}</span>}
                {r.firecrackerVersion && <span>fc {r.firecrackerVersion}</span>}
                {r.kernelVersion && <span>kernel {r.kernelVersion}</span>}
                <span>
                  stopped {r.stoppedCommands} · released {r.releasedWorkspaces}
                </span>
                {r.error && <span className="text-critical">{r.error}</span>}
              </div>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}
