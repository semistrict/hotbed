import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { clients, outputBytes } from "../api";

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
  // The poll loop and the terminal outlive `live` flipping to false: the loop
  // reads it each round and makes one last read when it is false, so the
  // terminal and its scroll position stay.
  const liveRef = useRef(live);
  const pollNow = useRef<(() => void) | null>(null);
  useEffect(() => {
    const was = liveRef.current;
    liveRef.current = live;
    if (was && !live) pollNow.current?.();
  }, [live]);

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
    let reading = false;
    let readAgain = false;
    const decoder = new TextDecoder();

    const read = async () => {
      if (reading) {
        readAgain = true; // a read is in flight: follow it with one that sees what it missed
        return;
      }
      reading = true;
      clearTimeout(timer);
      try {
        if (offset === null) {
          // Start near the end of a long output.
          const q = await clients.workspaces.queryOutput({ id, tail: 1, raw: true }, { signal: ac.signal });
          offset = q.totalBytes > REPLAY_BYTES ? q.totalBytes - REPLAY_BYTES : 0n;
          if (offset > 0n) term.writeln(`[hotbed: the first ${q.totalBytes - REPLAY_BYTES} bytes are not replayed here: hotbed output ${id} reads all of it]`);
        }
        const { data, next } = await outputBytes(id, offset, ac.signal);
        if (data.length > 0) term.write(decoder.decode(data, { stream: true }));
        offset = next;
        setError(null);
      } catch (e) {
        if (ac.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        reading = false;
      }
      if (ac.signal.aborted) return;
      if (readAgain) {
        readAgain = false;
        return read();
      }
      if (liveRef.current) timer = setTimeout(read, POLL_MS);
    };
    pollNow.current = () => void read();
    void read();
    return () => {
      pollNow.current = null;
      ac.abort();
      clearTimeout(timer);
      term.dispose();
    };
  }, [id]);

  return (
    <div className="space-y-2">
      <div ref={host} className="max-w-full overflow-x-auto rounded-lg border border-border bg-background p-2 font-mono" />
      {error && <div className="font-mono text-[11.5px] text-critical">Cannot read its output: {error}</div>}
    </div>
  );
}
