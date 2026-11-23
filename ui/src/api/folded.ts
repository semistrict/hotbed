import type { FoldedStack, ProfileFrame } from "./types";

/**
 * A frame as flamegraph.pl's folded format writes it: semicolons, which
 * separate frames there, become colons, and kernel frames end in "_[k]".
 */
export function frameLabel(f: ProfileFrame): string {
  let name = f.name.replaceAll(";", ":") || "[unknown]";
  if (f.frameType === "kernel" && !name.endsWith("_[k]")) name += "_[k]";
  return name;
}

/** A stack in flamegraph.pl's folded format, root first, without its count. */
export function foldStack(st: FoldedStack, frames: readonly ProfileFrame[]): string {
  return st.frames.map((id) => {
    const f = frames[id];
    return f ? frameLabel(f) : "[unknown]";
  }).join(";");
}
