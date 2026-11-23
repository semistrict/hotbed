import { CommandState } from "../api";

/** A command that has not ended: waiting, running, or frozen (parked for memory, or paused by its soft time limit). */
export function isLive(c: { state: CommandState }): boolean {
  return (
    c.state === CommandState.QUEUED ||
    c.state === CommandState.RUNNING ||
    c.state === CommandState.PARKED ||
    c.state === CommandState.PAUSED
  );
}
