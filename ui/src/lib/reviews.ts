import type { Finding, Location, Quick, Review, ReviewEvent, ReviewState, Severity } from "../api";

export const SEVERITIES: { severity: Severity; label: string; hint: string }[] = [
  { severity: "important", label: "Important", hint: "bugs the change adds or makes worse" },
  { severity: "pre-existing", label: "Pre-existing", hint: "bugs that were there before" },
  { severity: "nit", label: "Nits", hint: "minor issues" },
];

/** The findings under each severity that has any, worst first; undecided ones first within each. */
export function bySeverity(findings: Finding[]): { severity: Severity; findings: Finding[] }[] {
  return SEVERITIES.map(({ severity }) => ({
    severity,
    findings: findings.filter((f) => f.severity === severity).sort((a, b) => Number(a.decision !== "open") - Number(b.decision !== "open") || a.id - b.id),
  })).filter((g) => g.findings.length > 0);
}

/** The responses to a finding, or to the review as a whole (null), oldest first. */
export function thread(events: ReviewEvent[], findingId: number | null): ReviewEvent[] {
  return events.filter((e) => e.what === "responded" && e.findingId === findingId);
}

/** An excerpt's lines, numbered from the location's first line when it has one. */
export function numbered(l: Location): { n: number | null; text: string }[] {
  if (l.excerpt === "") return [];
  return l.excerpt.replace(/\n$/, "").split("\n").map((text, i) => ({ n: l.startLine > 0 ? l.startLine + i : null, text }));
}

/** Where a location points: path, path:12, path:12-18. */
export function where(l: Location): string {
  if (l.startLine === 0) return l.path;
  return l.endLine > l.startLine ? `${l.path}:${l.startLine}-${l.endLine}` : `${l.path}:${l.startLine}`;
}

/** A commit as people say it. */
export function short(commit: string): string {
  return commit.slice(0, 8);
}

/** The version reviewed in a line: 8d708783..6cb0ca1f, or 6cb0ca1f for the whole tree. */
export function versionLine(r: Review): string {
  return r.base ? `${short(r.base)}..${short(r.head)}` : `${short(r.head)} (whole tree)`;
}

export function ended(st: ReviewState): boolean {
  return st === "done" || st === "failed" || st === "cancelled";
}

export const QUICKS: { quick: Quick; label: string; hint: string }[] = [
  { quick: "verify", label: "Verify", hint: "Check that it is real" },
  { quick: "fix-if-verified", label: "Fix if verified", hint: "Check it, and fix it if it is real" },
  { quick: "wont-fix", label: "Won't fix", hint: "Leave it" },
];

export function quickLabel(q: Quick): string {
  return QUICKS.find((x) => x.quick === q)?.label ?? q;
}
