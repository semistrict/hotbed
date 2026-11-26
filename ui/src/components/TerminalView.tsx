import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { api } from "../api";

/** How much of the end of an output the terminal replays at first: its scrollback, roughly. */
const REPLAY_BYTES = 2n << 20n;
/** How often a live command's terminal reads what its output gained. */
const POLL_MS = 1000;

/** The cockpit's colors as a terminal theme: its tokens, read where it renders. */
function themeOf(el: HTMLElement) {
  const css = getComputedStyle(el);
  const v = (name: string) => css.getPropertyValue(name).trim() || undefined;
  return {
    background: v("--background"),
    foreground: v("--foreground"),
    cursor: v("--primary"),
    selectionBackground: v("--muted"),
  };
}

/**
 * A command's output as written, replayed in a terminal: what a person at
 * its terminal sees, colors and redraws included. A live one follows as it
 * writes; scrolling up holds the view where it is, as a terminal does.
 */
export function TerminalView({ id, live }: { id: string; live: boolean }) {
  const host = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const term = new Terminal({
      // The terminal the command ran in is 200 columns wide (guestd).
      cols: 200,
      rows: 30,
      convertEol: true,
      disableStdin: true,
      scrollback: 20000,
      fontSize: 11.5,
      fontFamily: getComputedStyle(el).fontFamily,
      theme: themeOf(el),
    });
    term.open(el);
    const ac = new AbortController();
    let offset: bigint | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const decoder = new TextDecoder();

    const read = async () => {
      try {
        if (offset === null) {
          // Start near the end of a long output.
          const q = await api.queryOutput(id, { tail: 1, raw: true });
          offset = q.totalBytes > REPLAY_BYTES ? q.totalBytes - REPLAY_BYTES : 0n;
          if (offset > 0n) term.writeln(`[hotbed: the first ${q.totalBytes - REPLAY_BYTES} bytes are not replayed here: hotbed output ${id} reads all of it]`);
        }
        const { data, next } = await api.outputBytes(id, offset, ac.signal);
        if (data.length > 0) term.write(decoder.decode(data, { stream: true }));
        offset = next;
        setError(null);
      } catch (e) {
        if (ac.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
      }
      if (live && !ac.signal.aborted) timer = setTimeout(read, POLL_MS);
    };
    void read();
    return () => {
      ac.abort();
      clearTimeout(timer);
      term.dispose();
    };
  }, [id, live]);

  return (
    <div className="space-y-2">
      <div ref={host} className="max-w-full overflow-x-auto rounded-lg border border-border bg-background p-2 font-mono" />
      {error && <div className="font-mono text-[11.5px] text-critical">Cannot read its output: {error}</div>}
    </div>
  );
}
