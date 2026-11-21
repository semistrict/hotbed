import type { CommandState } from "../api";

/** A command that has not ended: waiting, running, or frozen for memory. */
export function isLive(c: { state: CommandState }): boolean {
  return c.state === "queued" || c.state === "running" || c.state === "parked";
}
