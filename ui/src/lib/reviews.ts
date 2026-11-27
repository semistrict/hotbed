import {
  FindingDecision,
  FindingSeverity,
  QuickResponse,
  ReviewState,
  type Finding,
  type Location,
  type Review,
  type Reviewer,
  type ReviewEvent,
} from "../api";
import { shortSha } from "./format";

export const SEVERITIES: { severity: FindingSeverity; label: string; hint: string }[] = [
  { severity: FindingSeverity.IMPORTANT, label: "Important", hint: "bugs the change adds or makes worse" },
  { severity: FindingSeverity.PRE_EXISTING, label: "Pre-existing", hint: "bugs that were there before" },
  { severity: FindingSeverity.NIT, label: "Nits", hint: "minor issues" },
];

/** The findings under each severity that has any, worst first; undecided ones first within each. */
export function bySeverity(findings: Finding[]): { severity: FindingSeverity; findings: Finding[] }[] {
  return SEVERITIES.map(({ severity }) => ({
    severity,
    findings: findings
      .filter((f) => (f.severity === FindingSeverity.UNSPECIFIED ? FindingSeverity.NIT : f.severity) === severity)
      .sort(
        (a, b) =>
          Number(a.decision !== FindingDecision.OPEN && a.decision !== FindingDecision.UNSPECIFIED) -
            Number(b.decision !== FindingDecision.OPEN && b.decision !== FindingDecision.UNSPECIFIED) || a.id - b.id,
      ),
  })).filter((g) => g.findings.length > 0);
}

/** The responses to a finding, or to the review as a whole (0 / null), oldest first. */
export function thread(events: ReviewEvent[], findingId: number | null): ReviewEvent[] {
  const target = findingId ?? 0;
  return events.filter((e) => e.what === "responded" && e.findingId === target);
}

/** An excerpt's lines, numbered from the location's first line when it has one. */
export function numbered(l: Location): { n: number | null; text: string }[] {
  if (l.excerpt === "") return [];
  return l.excerpt
    .replace(/\n$/, "")
    .split("\n")
    .map((text, i) => ({ n: l.startLine > 0 ? l.startLine + i : null, text }));
}

/** Where a location points: path, path:12, path:12-18. */
export function where(l: Location): string {
  if (l.startLine === 0) return l.path;
  return l.endLine > l.startLine ? `${l.path}:${l.startLine}-${l.endLine}` : `${l.path}:${l.startLine}`;
}

/** A commit's page on its forge, when the repo (a canonical remote) is on GitHub; null otherwise. */
export function commitUrl(repo: string | null | undefined, commit: string): string | null {
  return repo?.startsWith("github.com/") ? `https://${repo}/commit/${commit}` : null;
}

/** Whether a finding is handled: fixed, so it takes no more responses. */
export function handled(f: Finding): boolean {
  return f.decision === FindingDecision.FIXED;
}

/** The version reviewed in a line: 8d708783..6cb0ca1f, or 6cb0ca1f for the whole tree. */
export function versionLine(r: Review): string {
  return r.base ? `${shortSha(r.base)}..${shortSha(r.head)}` : `${shortSha(r.head)} (whole tree)`;
}

export function ended(st: ReviewState): boolean {
  return st === ReviewState.DONE || st === ReviewState.FAILED || st === ReviewState.CANCELLED;
}

export const QUICKS: { quick: QuickResponse; label: string; hint: string }[] = [
  { quick: QuickResponse.VERIFY, label: "Verify", hint: "Check that it is real" },
  { quick: QuickResponse.FIX_IF_VERIFIED, label: "Fix if verified", hint: "Check it, and fix it if it is real" },
  { quick: QuickResponse.WONT_FIX, label: "Won't fix", hint: "Leave it" },
];

export function quickLabel(q: QuickResponse): string {
  return QUICKS.find((x) => x.quick === q)?.label ?? "";
}

/** Distinct reviewer models recorded on a review, in start order. */
export function reviewerModels(r: Review): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const rv of r.reviewers) {
    const m = rv.model.trim();
    if (m && !seen.has(m)) {
      seen.add(m);
      out.push(m);
    }
  }
  if (out.length === 0 && r.model.trim()) {
    for (const part of r.model.split(",")) {
      const m = part.trim();
      if (m && !seen.has(m)) {
        seen.add(m);
        out.push(m);
      }
    }
  }
  return out;
}

/** The Reviewer entry that reported a finding, if known. */
export function findingReviewer(r: Review, f: Finding): Reviewer | undefined {
  if (f.reviewerId > 0) {
    return r.reviewers.find((x) => x.id === f.reviewerId);
  }
  if (r.reviewers.length === 1) {
    return r.reviewers[0];
  }
  return undefined;
}
