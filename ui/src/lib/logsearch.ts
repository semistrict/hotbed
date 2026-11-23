// Searching a command's output: which lines hold the query, and where in each,
// so the log can show only those lines with the matches marked.

export interface LogMatch {
  /** 0-based index of the line in the log. */
  line: number;
  /** [start, end) of each match in the line, in order and never overlapping. */
  ranges: [number, number][];
}

/** The lines holding query, ignoring case. A blank query matches nothing. */
export function searchLog(lines: readonly string[], query: string): LogMatch[] {
  const q = query.toLowerCase();
  if (q.trim() === "") return [];
  const out: LogMatch[] = [];
  lines.forEach((text, line) => {
    const lower = text.toLowerCase();
    const ranges: [number, number][] = [];
    for (let i = lower.indexOf(q); i !== -1; i = lower.indexOf(q, i + q.length)) ranges.push([i, i + q.length]);
    if (ranges.length > 0) out.push({ line, ranges });
  });
  return out;
}
