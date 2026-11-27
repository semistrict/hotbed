import { useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "cn";
import { Button } from "@/components/ui/button";

/** Copy text to the clipboard, falling back to `document.execCommand("copy")` over non-HTTPS hosts. */
export async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to execCommand fallback (e.g. insecure LAN/tailnet HTTP or denied permission).
    }
  }
  if (typeof document === "undefined" || typeof document.execCommand !== "function") return false;
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  ta.style.pointerEvents = "none";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, text.length);
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  } finally {
    document.body.removeChild(ta);
  }
  return ok;
}

/** A small outline button that copies `text` and says "Copied" for a moment once it has. */
export function CopyButton({
  text,
  label = "Copy",
  copiedLabel = "Copied",
  icon,
  className,
}: {
  text: string;
  label?: string;
  copiedLabel?: string;
  icon?: ReactNode;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  const onCopy = () => {
    void copyToClipboard(text).then((ok) => {
      if (!ok) return;
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1800);
    });
  };

  return (
    <Button
      type="button"
      variant="outline"
      size="xs"
      onClick={onCopy}
      disabled={!text}
      title={`${label} to clipboard`}
      className={cn("font-mono text-[10.5px] text-muted-foreground hover:text-foreground", className)}
    >
      {icon}
      {copied ? copiedLabel : label}
    </Button>
  );
}
