// Oneof values several requests share, built the same way everywhere.

/** A list request's scope: one project's, or (null or empty) every project's. */
export function projectScope(project: string | null | undefined) {
  return project ? { case: "project" as const, value: project } : { case: "allProjects" as const, value: true };
}

/** Where a ranked item moves: just before another, or (null) last. */
export function placeBefore(before: number | null) {
  return before === null ? { case: "last" as const, value: true } : { case: "before" as const, value: before };
}
