import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { ErrorInfoSchema, NodeSchema } from "../gen/hotbed/v1/hotbed_pb";

vi.mock("@/components/ui/alert-dialog", () => ({
  AlertDialog: ({ open, children }: { open?: boolean; children?: ReactNode }) => (open ? <div data-open="true">{children}</div> : null),
  AlertDialogContent: ({ children }: { children?: ReactNode }) => <div data-slot="content">{children}</div>,
  AlertDialogHeader: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogTitle: ({ children }: { children?: ReactNode }) => <h2>{children}</h2>,
  AlertDialogDescription: ({ children }: { children?: ReactNode }) => <p>{children}</p>,
  AlertDialogFooter: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogCancel: ({ children, disabled }: { children?: ReactNode; disabled?: boolean }) => (
    <button type="button" disabled={disabled}>{children}</button>
  ),
  AlertDialogAction: ({ children, disabled, className }: { children?: ReactNode; disabled?: boolean; className?: string }) => (
    <button type="button" data-action="confirm" disabled={disabled} className={className}>{children}</button>
  ),
}));

import { DeleteNodeDialog, formatRetireNodeError } from "./DeleteNodeDialog";

function renderDialog(node: ReturnType<typeof create<typeof NodeSchema>> | null) {
  const qc = new QueryClient();
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <DeleteNodeDialog node={node} onOpenChange={vi.fn()} />
    </QueryClientProvider>,
  );
}

describe("formatRetireNodeError", () => {
  it("appends ErrorInfo suggestion from ConnectError details", () => {
    const err = new ConnectError("ubu1 is online: only a node gone for good is retired", Code.AlreadyExists, undefined, [
      {
        desc: ErrorInfoSchema,
        value: create(ErrorInfoSchema, {
          code: "conflict",
          suggestion: "stop its worker first: hotbed worker stop (or destroy) on its machine",
        }),
      },
    ]);
    expect(formatRetireNodeError(err)).toBe(
      "ubu1 is online: only a node gone for good is retired — stop its worker first: hotbed worker stop (or destroy) on its machine",
    );
  });

  it("formats ConnectError without ErrorInfo and plain errors cleanly", () => {
    expect(formatRetireNodeError(new ConnectError("permission denied", Code.PermissionDenied))).toBe("permission denied");
    expect(formatRetireNodeError(new Error("network down"))).toBe("network down");
  });
});

describe("DeleteNodeDialog", () => {
  it("renders confirmation copy and enabled solid destructive confirm action for an offline node", () => {
    const html = renderDialog(create(NodeSchema, { id: "n1", hostname: "old-box", online: false }));
    expect(html).toContain("Delete node old-box?");
    expect(html).toContain("Its command, event, and placement history stays");
    const confirmBtn = html.match(/<button\b[^>]*data-action="confirm"[^>]*>/)?.[0] ?? "";
    expect(confirmBtn).not.toContain("disabled");
    expect(confirmBtn).toContain("bg-destructive");
    expect(confirmBtn).toContain("text-destructive-foreground");
  });

  it("disables confirmation when the node is online", () => {
    const html = renderDialog(create(NodeSchema, { id: "n3", hostname: "ubu1", online: true }));
    expect(html).toContain("currently online and cannot be deleted");
    expect(html).toContain("hotbed worker stop");
    const confirmBtn = html.match(/<button\b[^>]*data-action="confirm"[^>]*>/)?.[0] ?? "";
    expect(confirmBtn).toContain("disabled");
  });
});
