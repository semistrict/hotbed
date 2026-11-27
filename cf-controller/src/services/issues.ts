import type { ServiceImpl } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import {
	IssueSchema,
	IssueService,
	IssueState,
	IssueUpvoteSchema,
	ListIssuesResponseSchema,
	type Issue,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import type { ControllerContext } from "../context.ts";
import { ErrorCode, hotbedError } from "../errors.ts";

const MAX_TITLE = 200;
const MAX_BODY = 20_000;
const MAX_COMMENT = 2_000;
/** An issue keeps its newest upvotes' comments up to this many; its votes count them all. */
const MAX_UPVOTES = 200;

/** Who someone is to an issue: a session, or for a person the host they came from. */
function voter(st?: Stamp): string {
	return st?.session ? `session:${st.session}` : `host:${st?.host ?? ""}`;
}

function who(st?: Stamp): string {
	return st?.session ? `session ${st.session}` : (st?.host ?? "someone");
}

/** Whether an issue holds every word of query, any case, in its title or body. */
export function issueMatches(issue: Issue, query: string): boolean {
	const text = `${issue.title}\n${issue.body}`.toLowerCase();
	return query
		.toLowerCase()
		.split(/\s+/)
		.filter(Boolean)
		.every((w) => text.includes(w));
}

export function createIssueService(ctrl: ControllerContext): ServiceImpl<typeof IssueService> {
	const touch = (issue: Issue, at: bigint) => {
		issue.updatedAt = at;
		ctrl.store.issues.save(issue);
		return issue;
	};
	return {
		reportIssue(req, ctx) {
			const title = req.title.trim();
			if (!title)
				throw hotbedError(ErrorCode.InvalidArgs, "an issue needs a title: one line, what is wrong");
			if (title.length > MAX_TITLE || title.includes("\n")) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`a title is one line of at most ${MAX_TITLE} characters`,
				).withSuggestion("put the rest in the body: -m DETAILS");
			}
			if (req.body.length > MAX_BODY) {
				throw hotbedError(ErrorCode.InvalidArgs, `a body is at most ${MAX_BODY} characters`);
			}
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			return ctrl.store.issues.add(
				create(IssueSchema, {
					title,
					body: req.body.trim(),
					state: IssueState.OPEN,
					reported: by,
					votes: 1,
					updatedAt: by.at,
					project: project?.name ?? "",
				}),
			);
		},

		listIssues(req) {
			if (req.archived && req.all) {
				throw hotbedError(ErrorCode.InvalidArgs, "--archived and --all: take one");
			}
			const state = req.all ? undefined : req.archived ? IssueState.ARCHIVED : IssueState.OPEN;
			const issues = ctrl.store.issues
				.list(state)
				.filter((i) => !req.query.trim() || issueMatches(i, req.query))
				// A listing is for finding one: its upvotes' comments are on its page.
				.map((i) => {
					i.upvotes = [];
					return i;
				});
			return create(ListIssuesResponseSchema, { issues });
		},

		getIssue(req) {
			return ctrl.store.issues.get(req.id);
		},

		upvoteIssue(req, ctx) {
			if (req.comment.length > MAX_COMMENT) {
				throw hotbedError(ErrorCode.InvalidArgs, `a comment is at most ${MAX_COMMENT} characters`);
			}
			const { by } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const issue = ctrl.store.issues.get(req.id);
			if (issue.state === IssueState.ARCHIVED) {
				const why = issue.archived?.reason ? `: ${issue.archived.reason}` : "";
				throw hotbedError(
					ErrorCode.Conflict,
					`issue ${issue.id} was archived by ${who(issue.archived)}${why}`,
				).withSuggestion(
					`if it is back, report it again, naming it: hotbed issue report "… (again: #${issue.id})"`,
				);
			}
			const me = voter(by);
			if (voter(issue.reported) === me || issue.upvotes.some((u) => voter(u.by) === me)) {
				throw hotbedError(
					ErrorCode.Conflict,
					`${who(by)} has counted for issue ${issue.id} already: each counts once`,
				);
			}
			issue.upvotes.push(create(IssueUpvoteSchema, { by, comment: req.comment.trim() }));
			if (issue.upvotes.length > MAX_UPVOTES)
				issue.upvotes.splice(0, issue.upvotes.length - MAX_UPVOTES);
			issue.votes += 1;
			return touch(issue, by.at);
		},

		archiveIssue(req, ctx) {
			const { by } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, req.reason.trim());
			const issue = ctrl.store.issues.get(req.id);
			if (issue.state === IssueState.ARCHIVED) return issue;
			issue.state = IssueState.ARCHIVED;
			issue.archived = by;
			return touch(issue, by.at);
		},

		reopenIssue(req, ctx) {
			const { by } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const issue = ctrl.store.issues.get(req.id);
			if (issue.state === IssueState.OPEN) return issue;
			issue.state = IssueState.OPEN;
			issue.archived = undefined;
			return touch(issue, by.at);
		},
	};
}
