import type { ServiceImpl } from "@connectrpc/connect";
import { create, fromJsonString } from "@bufbuild/protobuf";
import {
	AddFindingsRequestSchema,
	AddFindingsResponseSchema,
	DecideFindingsResponseSchema,
	FindingDecision,
	FindingSchema,
	FindingSeverity,
	FindingVerdict,
	ListResponsesResponseSchema,
	ListReviewsResponseSchema,
	LocationSchema,
	QuickResponse,
	ReviewEventSchema,
	ReviewSchema,
	ReviewService,
	ReviewState,
	type AddFindingsRequest,
	type DecideFindingsRequest,
	type Finding,
	type ReportedFinding,
	type Review,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import {
	inProject,
	MEMORY_FEEDBACK_REMINDER,
	MEMORY_FIX_REMINDER,
	stamped,
	type ControllerContext,
} from "../context.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { canonicalRemote, firstLine } from "../gitops.ts";

const MAX_TITLE = 60;
const COMMIT_RE = /^[0-9a-f]{7,64}$/;
const FULL_COMMIT_RE = /^([0-9a-f]{40}|[0-9a-f]{64})$/;
const FINDINGS_INFO = "json hotbed-findings";
const FENCE_OPEN = /^ {0,3}(`{3,}|~{3,})(.*)$/;
const FENCE_CLOSE = /^ {0,3}(`{3,}|~{3,})[ \t]*$/;
const REPORT_EXAMPLE = `end the report with\n\`\`\`${FINDINGS_INFO}\n{"findings": [{"summary": "...", "failure_scenario": "...", "severity": "important", "short_summary": "...", "file": "a.go", "line": 12}]}\n\`\`\`\n({"findings": []} when none survive)`;

function isReviewEnded(st: ReviewState): boolean {
	return st === ReviewState.DONE || st === ReviewState.FAILED || st === ReviewState.CANCELLED;
}

function reviewStateWord(st: ReviewState): string {
	switch (st) {
		case ReviewState.REQUESTED:
			return "requested";
		case ReviewState.IN_PROGRESS:
			return "in progress";
		case ReviewState.DONE:
			return "done";
		case ReviewState.FAILED:
			return "failed";
		case ReviewState.CANCELLED:
			return "cancelled";
		default:
			return "unspecified";
	}
}

function decisionWord(d: FindingDecision): string {
	switch (d) {
		case FindingDecision.OPEN:
			return "open";
		case FindingDecision.ACCEPTED:
			return "accepted";
		case FindingDecision.DISMISSED:
			return "dismissed";
		case FindingDecision.FIXED:
			return "fixed";
		case FindingDecision.INVALID:
			return "invalid";
		default:
			return "unspecified";
	}
}

function reviewerOf(r: Review, by: Stamp, doing: string): void {
	if (r.state === ReviewState.REQUESTED) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is not started: start it before you ${doing}`,
		).withSuggestion(
			`hotbed review start ${r.id} --model MODEL (your exact model ID, such as claude-opus-5-5)`,
		);
	}
	if (r.state !== ReviewState.IN_PROGRESS) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is ${reviewStateWord(r.state)}, ${stamped(r.ended)}`,
		);
	}
	if (r.started?.session !== by.session) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is ${r.started?.session ?? ""}'s to review; session ${by.session} may not ${doing}`,
		);
	}
}

function parseReportFindings(report: string): ReportedFinding[] {
	const lines = report.split("\n");
	let body: string[] = [];
	let found = false;
	let fence = "";
	let findings = false;
	let opened = 0;
	let inside: string[] = [];
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i].replace(/\r$/, "");
		if (!fence) {
			const m = FENCE_OPEN.exec(line);
			if (!m || (m[1][0] === "`" && m[2].includes("`"))) {
				continue;
			}
			fence = m[1];
			opened = i + 1;
			inside = [];
			findings = m[2].trim().split(/\s+/).filter(Boolean).join(" ") === FINDINGS_INFO;
			continue;
		}
		const cm = FENCE_CLOSE.exec(line);
		if (cm && cm[1][0] === fence[0] && cm[1].length >= fence.length) {
			if (findings) {
				body = inside;
				found = true;
			}
			fence = "";
			continue;
		}
		inside.push(line);
	}
	if (fence && findings) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`the report's \`\`\`${FINDINGS_INFO} block at line ${opened} is never closed`,
		).withSuggestion(REPORT_EXAMPLE);
	}
	if (!found) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`the report has no \`\`\`${FINDINGS_INFO} block, so it says nothing of its findings`,
		).withSuggestion(REPORT_EXAMPLE);
	}
	let payload: AddFindingsRequest;
	try {
		payload = fromJsonString(AddFindingsRequestSchema, body.join("\n"));
	} catch (err) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`the report's \`\`\`${FINDINGS_INFO} block is not {"findings": [...]}: ${err instanceof Error ? err.message : String(err)}`,
		).withSuggestion(REPORT_EXAMPLE);
	}
	if (payload.reviewId !== 0 || payload.sessionId || payload.report) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`the report's \`\`\`${FINDINGS_INFO} block holds findings alone`,
		).withSuggestion(REPORT_EXAMPLE);
	}
	return payload.findings;
}

function toFinding(rf: ReportedFinding): Finding {
	const summary = rf.summary.trim();
	if (!summary) {
		throw hotbedError(ErrorCode.InvalidArgs, "a finding needs its summary: the defect");
	}
	const sevMap: Record<string, FindingSeverity> = {
		important: FindingSeverity.IMPORTANT,
		pre_existing: FindingSeverity.PRE_EXISTING,
		nit: FindingSeverity.NIT,
	};
	const severity = sevMap[rf.severity];
	if (severity === undefined) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`severity "${rf.severity}": give important, pre_existing or nit`,
		);
	}
	const verdMap: Record<string, FindingVerdict> = {
		"": FindingVerdict.UNSPECIFIED,
		CONFIRMED: FindingVerdict.CONFIRMED,
		PLAUSIBLE: FindingVerdict.PLAUSIBLE,
	};
	const verdict = verdMap[rf.verdict];
	if (verdict === undefined) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`verdict "${rf.verdict}": give CONFIRMED, PLAUSIBLE, or none`,
		);
	}
	let title = rf.shortSummary.trim() || firstLine(summary);
	const runes = Array.from(title);
	if (runes.length > MAX_TITLE) {
		if (rf.shortSummary) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`short_summary "${rf.shortSummary}" is longer than ${MAX_TITLE} characters`,
			);
		}
		title = `${runes.slice(0, MAX_TITLE - 1).join("")}…`;
	}
	const locations = [];
	if (rf.file) {
		locations.push(create(LocationSchema, { path: rf.file, startLine: rf.line }));
	} else if (rf.line !== 0) {
		throw hotbedError(ErrorCode.InvalidArgs, `line ${rf.line} names no file`);
	}
	for (const l of rf.locations) {
		if (!l.path.trim()) {
			throw hotbedError(ErrorCode.InvalidArgs, "a location needs its path");
		}
		if (l.startLine < 0 || l.endLine < 0) {
			throw hotbedError(ErrorCode.InvalidArgs, `${l.path}: lines count from 1`);
		}
		if (l.endLine !== 0 && l.endLine < l.startLine) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`${l.path}: lines ${l.startLine} to ${l.endLine} run backwards`,
			);
		}
		if (
			locations.length === 1 &&
			rf.file === l.path &&
			(rf.line === 0 || rf.line === l.startLine)
		) {
			locations[0] = l;
			continue;
		}
		locations.push(l);
	}
	return create(FindingSchema, {
		title,
		summary,
		failureScenario: rf.failureScenario,
		category: rf.category,
		severity,
		verdict,
		locations,
	});
}

function checkDecision(req: DecideFindingsRequest): void {
	const fixed = req.decision === FindingDecision.FIXED;
	const invalid = req.decision === FindingDecision.INVALID;
	if (req.fix && !fixed) {
		throw hotbedError(ErrorCode.InvalidArgs, "a fix is given only with fixed");
	}
	if (req.evidence && !invalid) {
		throw hotbedError(ErrorCode.InvalidArgs, "evidence is given only with invalid");
	}
	if (invalid && !req.evidence.trim()) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			"say why the findings are invalid: the evidence that they are not real",
		).withSuggestion("hotbed review invalid ID FINDING --evidence TEXT (or --evidence-file FILE)");
	}
	if (fixed && !FULL_COMMIT_RE.test(req.fix?.commit ?? "")) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`fixed in "${req.fix?.commit ?? ""}": name the commit that fixed them in full (40 or 64 hex digits)`,
		).withSuggestion("hotbed review fixed ID FINDING --commit REF resolves REF in this checkout");
	}
	for (const l of req.fix?.locations ?? []) {
		if (!l.path.trim()) {
			throw hotbedError(ErrorCode.InvalidArgs, "a fix's location needs its path");
		}
		if (l.startLine < 0 || l.endLine < 0 || (l.endLine !== 0 && l.endLine < l.startLine)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`${l.path}: lines ${l.startLine} to ${l.endLine} are not lines of a file`,
			);
		}
	}
}

export function createReviewService(ctrl: ControllerContext): ServiceImpl<typeof ReviewService> {
	return {
		requestReview(req, ctx) {
			if (!COMMIT_RE.test(req.head)) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`head "${req.head}" is not a commit: a review is of an exact version`,
				).withSuggestion(
					"hotbed review request, in a checkout, names its HEAD (or a snapshot of its changes) itself",
				);
			}
			for (const c of [req.base, req.snapshotOf]) {
				if (c && !COMMIT_RE.test(c)) {
					throw hotbedError(ErrorCode.InvalidArgs, `"${c}" is not a commit`);
				}
			}
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			const title = req.title.trim() || `${req.branch || "detached"} at ${req.head.slice(0, 8)}`;
			const review = create(ReviewSchema, {
				repo: canonicalRemote(req.repo),
				title,
				instructions: req.instructions,
				base: req.base,
				head: req.head,
				branch: req.branch,
				snapshotOf: req.snapshotOf,
				state: ReviewState.REQUESTED,
				requested: by,
			});
			return ctrl.store.addReview(project.name, review);
		},

		listReviews(req) {
			if (req.limit < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `limit ${req.limit}: give 0 for all, or how many`);
			}
			const project = ctrl.listedProject(req);
			return create(ListReviewsResponseSchema, {
				reviews: ctrl.store.listReviews(project, req.states, req.limit),
			});
		},

		getReview(req) {
			return ctrl.store.getReview(req.id, true);
		},

		startReview(req, ctx) {
			const model = req.model.trim();
			if (!model) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`starting review ${req.id} names no model`,
				).withSuggestion(
					`pass the reviewer's exact model ID, as its provider names it: hotbed review start ${req.id} --model claude-opus-5-5`,
				);
			}
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			try {
				const existing = ctrl.store.getReview(req.id, false);
				if (
					existing.state === ReviewState.IN_PROGRESS &&
					existing.started?.session === by.session
				) {
					if (existing.model !== model) {
						throw hotbedError(
							ErrorCode.Conflict,
							`review ${existing.id} was started by model ${existing.model || "unknown"}, not ${model}`,
						).withSuggestion(
							"a review is one model's: cancel it and request another for a different reviewer",
						);
					}
					return ctrl.store.getReview(req.id, true);
				}
			} catch (err) {
				if (err instanceof Error && "code" in err && err.code !== ErrorCode.NotFound) throw err;
			}
			ctrl.store.changeReview(req.id, "started", by, (r) => {
				inProject("review", r.id, r.project, project, by);
				if (r.state !== ReviewState.REQUESTED) {
					const byStr = r.started?.session ? ` by ${r.started.session}` : "";
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} is ${reviewStateWord(r.state)}${byStr}; only a requested review can be started`,
					);
				}
				r.state = ReviewState.IN_PROGRESS;
				r.started = by;
				r.model = model;
			});
			return ctrl.store.getReview(req.id, true);
		},

		addFindings(req, ctx) {
			let reported: ReportedFinding[];
			if (!req.report) {
				reported = req.findings;
			} else if (req.findings.length > 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"give findings or a report that ends with them, not both",
				);
			} else {
				reported = parseReportFindings(req.report);
			}
			const findings = reported.map((rf, i) => {
				try {
					return toFinding(rf);
				} catch (err) {
					const msg = err instanceof Error ? err.message : String(err);
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`finding ${i + 1} of ${reported.length}: ${msg}`,
					);
				}
			});
			const { by } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			const out = ctrl.store.addFindings(
				req.reviewId,
				by,
				(r) => reviewerOf(r, by, "report findings"),
				findings,
				req.report.trim(),
			);
			return create(AddFindingsResponseSchema, { findings: out });
		},

		finishReview(req, ctx) {
			const what = req.failed ? "failed" : "finished";
			const state = req.failed ? ReviewState.FAILED : ReviewState.DONE;
			if (req.failed && !req.summary.trim()) {
				throw hotbedError(ErrorCode.InvalidArgs, "say why the review failed").withSuggestion(
					"hotbed review fail ID --reason WHY",
				);
			}
			const { by } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			const r = ctrl.store.changeReview(req.id, what, by, (rev) => {
				reviewerOf(rev, by, "end it");
				rev.state = state;
				rev.summary = req.summary;
				rev.ended = by;
			});
			if (r.requested?.session) ctrl.sessionWakes.wake(r.requested.session);
			return ctrl.store.getReview(req.id, true);
		},

		cancelReview(req, ctx) {
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, req.reason);
			ctrl.store.changeReview(req.id, "cancelled", by, (r) => {
				inProject("review", r.id, r.project, project, by);
				if (isReviewEnded(r.state)) {
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} is ${reviewStateWord(r.state)} already, ${stamped(r.ended)}`,
					);
				}
				r.state = ReviewState.CANCELLED;
				r.summary = req.reason;
				r.ended = by;
			});
			return ctrl.store.getReview(req.id, true);
		},

		decideFindings(req, ctx) {
			const whatMap: Partial<Record<FindingDecision, string>> = {
				[FindingDecision.ACCEPTED]: "accepted",
				[FindingDecision.DISMISSED]: "dismissed",
				[FindingDecision.FIXED]: "fixed",
				[FindingDecision.INVALID]: "invalid",
				[FindingDecision.OPEN]: "reopened",
			};
			const what = whatMap[req.decision];
			if (!what) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"decide a finding accepted, dismissed, fixed, invalid or open",
				);
			}
			if (req.findingIds.length === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, "name the findings").withSuggestion(
					`hotbed review show ${req.reviewId}`,
				);
			}
			checkDecision(req);
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, req.reason);
			const findings = ctrl.store.changeFindings(req.reviewId, req.findingIds, what, by, (r, f) => {
				inProject("review", r.id, r.project, project, by);
				if (f.decision === req.decision) {
					throw hotbedError(
						ErrorCode.Conflict,
						f.decided
							? `finding ${f.id} is ${decisionWord(f.decision)} already, ${stamped(f.decided)}`
							: `finding ${f.id} is ${decisionWord(f.decision)} already`,
					);
				}
				if (f.decision === FindingDecision.FIXED && req.decision !== FindingDecision.OPEN) {
					throw hotbedError(
						ErrorCode.Conflict,
						`finding ${f.id} is fixed, in ${f.fix?.commit ?? ""}: it is handled`,
					).withSuggestion(`hotbed review reopen ${r.id} ${f.id}, if the fix did not hold`);
				}
				if (
					req.decision === FindingDecision.FIXED &&
					(f.decision === FindingDecision.DISMISSED || f.decision === FindingDecision.INVALID)
				) {
					throw hotbedError(
						ErrorCode.Conflict,
						`finding ${f.id} is ${decisionWord(f.decision)}: reopen it before you mark it fixed`,
					).withSuggestion(`hotbed review reopen ${r.id} ${f.id}`);
				}
				f.decision = req.decision;
				f.decided = req.decision === FindingDecision.OPEN ? undefined : by;
				f.fix = req.fix;
				f.evidence = req.evidence;
			});
			return create(DecideFindingsResponseSchema, {
				findings,
				...(req.decision === FindingDecision.FIXED ? { reminder: MEMORY_FIX_REMINDER } : {}),
			});
		},

		respond(req, ctx) {
			if (req.quick === QuickResponse.UNSPECIFIED && !req.text.trim()) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"a response gives a quick response, a comment, or both",
				);
			}
			if (req.quick !== QuickResponse.UNSPECIFIED && req.findingId === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, "a quick response is to a finding: name it");
			}
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const ev = create(ReviewEventSchema, {
				reviewId: req.reviewId,
				findingId: req.findingId,
				by,
				quick: req.quick,
				text: req.text,
			});
			const saved = ctrl.store.addReviewResponse(ev, req.after, (r, f) => {
				inProject("review", r.id, r.project, project, by);
				if (f?.decision === FindingDecision.FIXED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`finding ${f.id} is fixed, in ${f.fix?.commit ?? ""}: it is handled, and takes no more responses`,
					).withSuggestion(
						`hotbed review reopen ${r.id} ${f.id}, if the fix did not hold; or respond to the review as a whole`,
					);
				}
			});
			const r = ctrl.store.getReview(req.reviewId, false);
			if (r.requested?.session) ctrl.sessionWakes.wake(r.requested.session);
			return saved;
		},

		listResponses(req) {
			ctrl.store.getReview(req.reviewId, false);
			const events = ctrl.store.reviewEventsAfter(req.reviewId, req.after);
			let cursor = req.after;
			const responses = [];
			for (const e of events) {
				if (e.seq > cursor) cursor = e.seq;
				if (e.what === "responded") responses.push(e);
			}
			return create(ListResponsesResponseSchema, {
				responses,
				cursor,
				...(responses.length > 0 ? { reminder: MEMORY_FEEDBACK_REMINDER } : {}),
			});
		},
	};
}
