import { ConnectError } from "@connectrpc/connect";
import { useEffect } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import type { Node } from "../api";
import { ErrorInfoSchema } from "../gen/hotbed/v1/hotbed_pb";
import { useRetireNode } from "../lib/data";

export function formatRetireNodeError(err: unknown): string {
  if (err instanceof ConnectError) {
    const info = err.findDetails(ErrorInfoSchema)[0];
    const msg = err.rawMessage || err.message;
    if (info?.suggestion) {
      return `${msg} — ${info.suggestion}`;
    }
    return msg;
  }
  return err instanceof Error ? err.message : String(err);
}

export interface DeleteNodeDialogProps {
  node: Node | null;
  onOpenChange: (open: boolean) => void;
  onDeleted?: (node: Node) => void;
}

export function DeleteNodeDialog({ node, onOpenChange, onDeleted }: DeleteNodeDialogProps) {
  const retire = useRetireNode();
  const nodeId = node?.id ?? null;

  useEffect(() => {
    retire.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodeId]);

  function handleOpenChange(open: boolean) {
    if (!open && retire.isPending) return;
    if (!open) retire.reset();
    onOpenChange(open);
  }

  function handleConfirm() {
    if (!node || node.online || retire.isPending) return;
    retire.mutate(node.id, {
      onSuccess: (retired) => {
        onOpenChange(false);
        onDeleted?.(retired);
      },
    });
  }

  return (
    <AlertDialog open={node !== null} onOpenChange={handleOpenChange}>
      <AlertDialogContent className="sm:max-w-md">
        {node && (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete node {node.hostname}?</AlertDialogTitle>
              <AlertDialogDescription className="space-y-2">
                <span className="block">
                  Removes <span className="font-mono text-foreground">{node.hostname}</span> from the cluster and
                  cockpit so it no longer appears or takes runs. Its command, event, and placement history stays, and if
                  a worker joins again with the same identity it will reappear.
                </span>
                {node.asleep && !node.online && (
                  <span className="block text-warning-ink">
                    This node is currently asleep under standby. If its host wakes it, it will rejoin automatically.
                  </span>
                )}
                {node.online && (
                  <span className="block text-critical">
                    This node is currently online and cannot be deleted. Stop its worker first (
                    <span className="font-mono">hotbed worker stop</span>) on its machine.
                  </span>
                )}
              </AlertDialogDescription>
            </AlertDialogHeader>
            {retire.error && (
              <Alert variant="destructive" className="border-critical/40 font-mono text-[11px]">
                <AlertDescription className="text-[11px] text-critical">
                  {formatRetireNodeError(retire.error)}
                </AlertDescription>
              </Alert>
            )}
            <AlertDialogFooter>
              <AlertDialogCancel disabled={retire.isPending}>Cancel</AlertDialogCancel>
              <AlertDialogAction
                type="button"
                variant="destructive"
                disabled={retire.isPending || node.online}
                onClick={handleConfirm}
                className="bg-destructive text-destructive-foreground hover:bg-destructive/90 dark:bg-destructive dark:hover:bg-destructive/90"
              >
                {retire.isPending ? "Deleting…" : "Delete node"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </>
        )}
      </AlertDialogContent>
    </AlertDialog>
  );
}
