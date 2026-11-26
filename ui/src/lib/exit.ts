import { CommandState, type FinishedCommand } from "../api";

type Ended = Pick<FinishedCommand, "exitCode" | "state" | "failure" | "reason">;

// endedExit is an ended command's exit code as it reads: "signal" when a
// signal ended it, "—" when it never exited (cancelled, or its VM ended
// under it, so whatever ended it was not the command's own exit).
export function endedExit(done: Ended): string {
  if (done.exitCode !== undefined) return String(done.exitCode);
  if (done.state === CommandState.SUCCEEDED) return "0";
  if (done.state === CommandState.CANCELLED || done.failure === "vm") return "—";
  return "signal";
}

// endedHow is how a failed command ended, beside its failure's kind: its
// exit, or, when its VM ended under it, how the VM did.
export function endedHow(done: Ended): string {
  return done.failure === "vm" ? done.reason : `exit ${endedExit(done)}`;
}
