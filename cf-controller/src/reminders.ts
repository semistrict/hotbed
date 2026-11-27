// The reminders the controller appends to its answers to agents.
import type { Project } from "./gen/hotbed/v1/hotbed_pb.ts";

export function setupStepsReminder(p?: Project | null): string {
	const standard =
		"By default, after all setup steps have run, common dev commands (build, test, check, lint, etc.) should run without downloading or compiling anything.";
	if (!p || !p.name) {
		return (
			"Reminder: add setup steps (`hotbed step ls -p PROJECT`, `hotbed step add NAME -- ...`, `hotbed step set -p PROJECT NAME -- ...`; use `--every-pull` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. " +
			standard
		);
	}
	if (p.steps.length === 0) {
		return `Reminder: project ${p.name} has no setup steps yet. Add setup steps (\`hotbed step add NAME -- ...\` or \`hotbed step set -p ${p.name} NAME -- ...\`; use \`--every-pull\` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. ${standard}`;
	}
	return `Reminder: make sure project ${p.name} has setup steps (\`hotbed step ls -p ${p.name}\`, \`hotbed step add\`, \`hotbed step set\`; use \`--every-pull\` for dependency installs and builds) for common things like toolchain installs, dependencies, and pre-builds. ${standard}`;
}

// Where a lesson goes. Every memory's summary costs every session's context,
// so the reminders below send a lesson to the place it will be found first
// (its fix, the repo, a skill) and to memory only when it has no such home.
const LESSON_HOMES =
	'fix its cause, or write it where it will be found (the repo\'s AGENTS.md, a comment by the code, the tool\'s skill); save a memory only for what has no such home (a preference, a machine fact, a workaround naming its open bug): `hotbed memory ls -q WORD` for one to update, else `hotbed memory add --slug SLUG --summary "when it applies" --body "..." --rationale "..."` (MCP: `memory_add`).';

export const MEMORY_FAILURE_REMINDER = `Reminder: if this failure revealed a reusable fix or quirk, ${LESSON_HOMES}`;

export const MEMORY_FEEDBACK_REMINDER = `Reminder: if this user answer or review feedback reflects a standing preference or convention, ${LESSON_HOMES}`;

export const MEMORY_FIX_REMINDER = `Reminder: if this defect exposed a reusable pitfall or invariant, ${LESSON_HOMES}`;

export const MEMORY_SESSION_END_REMINDER = `Before wrapping up: if this session taught something reusable, ${LESSON_HOMES} Archive memories it made obsolete with \`hotbed memory archive ID --reason "..."\`.`;

export function memorySavingReminder(): string {
	return `Lessons: ${LESSON_HOMES} Archive obsolete memories with \`hotbed memory archive ID --reason "..."\`.`;
}

/** How an agent reports a problem with hotbed itself, once, and counts its own hitting of one already reported. */
export const ISSUE_REMINDER =
	"Hotbed itself failing you (a bug, a missing capability, a misleading error)? `hotbed issue ls -q WORD`: +1 one already reported (`hotbed issue upvote ID -m WHAT`), else `hotbed issue report TITLE -m DETAILS`.";

export function sessionStartReminder(p?: Project | null): string {
	return `${setupStepsReminder(p)}\n${memorySavingReminder()}\n${ISSUE_REMINDER}`;
}
