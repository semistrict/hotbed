// What questions and memories, both items filed under a project (and maybe
// one of its repos) and edited by revision, share.
import type { Project } from "../gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";

/**
 * The project repo an item is filed under: the one named, which the project
 * must hold, or else the checkout it came from when the project holds that,
 * or none.
 */
export function projectRepo(project: Project, named: string, fromRemote: string): string {
	if (named) {
		const repo = canonicalRemote(named);
		if (!project.repos.includes(repo)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`project ${project.name} does not hold ${repo}; it holds ${project.repos.join(", ")}`,
			).withSuggestion(`hotbed project add ${project.name} --repo ${repo}`);
		}
		return repo;
	}
	const from = canonicalRemote(fromRemote);
	if (from && project.repos.includes(from)) return from;
	return "";
}

/**
 * Refuses an edit made against another revision of the item (want 0 edits
 * whatever revision it is at). `item` names it, `show` is how to see it.
 */
export function checkRevision(revision: bigint, want: bigint, item: string, show: string): void {
	if (want > 0n && revision !== want) {
		throw hotbedError(
			ErrorCode.Conflict,
			`${item} is at revision ${revision}, not ${want}`,
		).withSuggestion(show);
	}
}
