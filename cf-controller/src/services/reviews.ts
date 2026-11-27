import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create, fromJsonString } from "@bufbuild/protobuf";
import {
	ActorKind,
	AddFindingsRequestSchema,
	AddFindingsResponseSchema,
	DecideFindingsResponseSchema,
	FindingDecision,
	FindingDecisionSchema,
	FindingSchema,
	FindingSeverity,
	FindingVerdict,
	ListResponsesResponseSchema,
	ListReviewsResponseSchema,
	LocationSchema,
	QuickResponse,
	ReviewerSchema,
	ReviewerState,
	ReviewerStateSchema,
	ReviewEventSchema,
	ReviewSchema,
	ReviewService,
	ReviewState,
	ReviewStateSchema,
	ReviewUpdate_What,
	StampSchema,
	type AddFindingsRequest,
	type DecideFindingsRequest,
	type Finding,
	type ReportedFinding,
	type Review,
	type Reviewer,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumWord } from "../enums.ts";
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

function decisionWord(d: FindingDecision): string {
	return d === FindingDecision.WONT_FIX ? "won't fix" : enumWord(FindingDecisionSchema, d);
}

function reviewerLabel(rv: Reviewer): string {
	const parts = [`#${rv.id}`, rv.model || "unknown"];
	if (rv.subagent) parts.push(`(${rv.subagent})`);
	return parts.join(" ");
}

function joinReviewerModels(reviewers: Reviewer[]): string {
	const seen = new Set<string>();
	const out: string[] = [];
	for (const rv of reviewers) {
		const m = rv.model.trim();
		if (m && !seen.has(m)) {
			seen.add(m);
			out.push(m);
		}
	}
	return out.join(", ");
}

function resolveActiveReviewer(
	r: Review,
	by: Stamp,
	sel: { reviewerId: number; model: string; subagent: string },
	doing: string,
): Reviewer {
	if (r.state === ReviewState.CANCELLED) {
		throw hotbedError(ErrorCode.Conflict, `review ${r.id} is cancelled, ${stamped(r.ended)}`);
	}
	if (r.reviewers.length === 0) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is not started: start it before you ${doing}`,
		).withSuggestion(
			`hotbed review start ${r.id} --model MODEL (your exact model ID, such as claude-opus-4-6)`,
		);
	}
	if (sel.reviewerId > 0) {
		const rv = r.reviewers.find((x) => x.id === sel.reviewerId);
		if (!rv) {
			throw hotbedError(ErrorCode.NotFound, `review ${r.id} has no reviewer #${sel.reviewerId}`);
		}
		if (rv.session !== by.session) {
			throw hotbedError(
				ErrorCode.Conflict,
				`reviewer #${rv.id} of review ${r.id} is ${rv.session}'s; session ${by.session} may not ${doing}`,
			);
		}
		if (rv.state !== ReviewerState.IN_PROGRESS) {
			throw hotbedError(
				ErrorCode.Conflict,
				`reviewer #${rv.id} (${rv.model || "unknown"}) of review ${r.id} is ${enumWord(ReviewerStateSchema, rv.state)}, ${stamped(rv.ended)}`,
			);
		}
		return rv;
	}
	const inProgress = r.reviewers.filter((x) => x.state === ReviewerState.IN_PROGRESS);
	if (inProgress.length === 0) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is ${enumWord(ReviewStateSchema, r.state)}, ${stamped(r.ended)}`,
		);
	}
	let mine = inProgress.filter((x) => x.session === by.session);
	const model = sel.model.trim();
	const subagent = sel.subagent.trim();
	if (model) mine = mine.filter((x) => x.model === model);
	if (subagent) mine = mine.filter((x) => x.subagent === subagent);
	if (mine.length === 1) return mine[0];
	if (mine.length > 1) {
		const labels = mine.map(reviewerLabel).join(", ");
		throw hotbedError(
			ErrorCode.Conflict,
			`session ${by.session} has ${mine.length} active reviewers on review ${r.id} (${labels}): name which one`,
		).withSuggestion(`pass --reviewer ${mine[0].id} (or --model / --subagent) to pick one`);
	}
	const owners = [...new Set(inProgress.map((x) => x.session))];
	if (owners.length === 1 && owners[0] !== by.session) {
		throw hotbedError(
			ErrorCode.Conflict,
			`review ${r.id} is ${owners[0]}'s to review; session ${by.session} may not ${doing}`,
		);
	}
	if (model || subagent) {
		throw hotbedError(
			ErrorCode.Conflict,
			`no active reviewer matching ${[model, subagent].filter(Boolean).join(" / ")} for session ${by.session} on review ${r.id}`,
		).withSuggestion(`active reviewers: ${inProgress.map(reviewerLabel).join(", ")}`);
	}
	throw hotbedError(
		ErrorCode.Conflict,
		`review ${r.id} is ${owners.join(", ")}'s to review; session ${by.session} may not ${doing}`,
	);
}

function stampForReviewer(by: Stamp, rv: Reviewer): Stamp {
	const out = clone(StampSchema, by);
	out.actorKind = ActorKind.AGENT;
	out.model = rv.model || out.model;
	out.subagent = rv.subagent || out.subagent;
	if (!out.t3codeThreadId) out.t3codeThreadId = rv.t3codeThreadId;
	return out;
}

function enrichImplementerStamp(r: Review, by: Stamp): Stamp {
	if (by.actorKind !== ActorKind.AGENT) return by;
	const out = clone(StampSchema, by);
	if (
		!out.model &&
		out.session &&
		(out.session === r.implementer?.session || out.session === r.requested?.session)
	) {
		out.model = r.implementerModel || r.implementer?.model || "";
	}
	if (
		!out.subagent &&
		out.session &&
		out.session === r.implementer?.session &&
		r.implementer?.subagent
	) {
		out.subagent = r.implementer.subagent;
	}
	return out;
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
			const caller = getCaller(ctx.values);
			const { by, project, session } = ctrl.agentStamp(caller, req.sessionId, "", {
				model: req.model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});

			let implementer = clone(StampSchema, by);
			const implSessionId = req.implementerSession.trim();
			if (implSessionId && implSessionId !== req.sessionId) {
				const implSess = ctrl.store.sessions.get(implSessionId);
				if (implSess.project !== project.name) {
					throw hotbedError(
						ErrorCode.Conflict,
						`implementer session ${implSess.id} works on project ${implSess.project}, not ${project.name}`,
					);
				}
				implementer = create(StampSchema, {
					at: by.at,
					actorKind: ActorKind.AGENT,
					session: implSess.id,
					host: caller.hostname,
					model: req.model.trim() || implSess.model,
					subagent: req.subagent.trim(),
					t3codeThreadId: implSess.t3codeThreadId || by.t3codeThreadId,
				});
			} else if (!implementer.model) {
				implementer.model = session.model;
			}

			const implementerModel = implementer.model.trim();
			if (!implementerModel) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"requesting a review names no implementer model",
				).withSuggestion(
					"pass the implementer's exact model ID: hotbed review request --model MODEL (or start the session with --model MODEL)",
				);
			}
			implementer.model = implementerModel;
			if (!by.model) by.model = implementerModel;

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
				implementer,
				implementerModel,
			});
			return ctrl.store.reviews.add(project.name, review);
		},

		listReviews(req) {
			if (req.limit < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `limit ${req.limit}: give 0 for all, or how many`);
			}
			const project = ctrl.listedProject(req);
			return create(ListReviewsResponseSchema, {
				reviews: ctrl.store.reviews.list(project, req.states, req.limit),
			});
		},

		getReview(req) {
			return ctrl.store.reviews.get(req.id, true);
		},

		startReview(req, ctx) {
			const model = req.model.trim();
			if (!model) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`starting review ${req.id} names no model`,
				).withSuggestion(
					`pass the reviewer's exact model ID, as its provider names it: hotbed review start ${req.id} --model claude-opus-4-6`,
				);
			}
			const caller = getCaller(ctx.values);
			const { by, project } = ctrl.agentStamp(caller, req.sessionId, "", {
				model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});
			const subagent = by.subagent.trim();

			// Idempotent retry: same session, same model, same subagent already in progress (unless --new).
			if (!req.newReviewer) {
				try {
					const existing = ctrl.store.reviews.get(req.id, false);
					if (existing.state !== ReviewState.CANCELLED) {
						const same = existing.reviewers.find(
							(rv) =>
								rv.state === ReviewerState.IN_PROGRESS &&
								rv.session === by.session &&
								rv.model === model &&
								rv.subagent === subagent,
						);
						if (same) {
							return ctrl.store.reviews.get(req.id, true);
						}
					}
				} catch (err) {
					if (err instanceof Error && "code" in err && err.code !== ErrorCode.NotFound) throw err;
				}
			}

			ctrl.store.reviews.change(req.id, "started", by, (r) => {
				inProject("review", r.id, r.project, project, by);
				if (r.state === ReviewState.CANCELLED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} is cancelled, ${stamped(r.ended)}; a cancelled review cannot be started`,
					);
				}
				const nextId = r.reviewers.reduce((max, x) => Math.max(max, x.id), 0) + 1;
				const rv = create(ReviewerSchema, {
					id: nextId,
					model,
					subagent,
					session: by.session,
					t3codeThreadId: by.t3codeThreadId,
					state: ReviewerState.IN_PROGRESS,
					started: by,
				});
				r.reviewers.push(rv);
				r.state = ReviewState.IN_PROGRESS;
				if (!r.started) r.started = by;
				r.ended = undefined;
				r.model = joinReviewerModels(r.reviewers);
				return { reviewerId: rv.id, stamp: by };
			});
			return ctrl.store.reviews.get(req.id, true);
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
			const { by } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "", {
				model: req.model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});
			const out = ctrl.store.reviews.addFindings(
				req.reviewId,
				by,
				(r) => {
					const rv = resolveActiveReviewer(
						r,
						by,
						{ reviewerId: req.reviewerId, model: req.model, subagent: req.subagent },
						"report findings",
					);
					return { reviewerId: rv.id, stamp: stampForReviewer(by, rv) };
				},
				findings,
				req.report.trim(),
			);
			return create(AddFindingsResponseSchema, { findings: out });
		},

		finishReview(req, ctx) {
			const what = req.failed ? "failed" : "finished";
			if (req.failed && !req.summary.trim()) {
				throw hotbedError(ErrorCode.InvalidArgs, "say why the review failed").withSuggestion(
					"hotbed review fail ID --reason WHY",
				);
			}
			const { by } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "", {
				model: req.model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});
			const r = ctrl.store.reviews.change(req.id, what, by, (rev) => {
				const rv = resolveActiveReviewer(
					rev,
					by,
					{ reviewerId: req.reviewerId, model: req.model, subagent: req.subagent },
					"end it",
				);
				const stamp = stampForReviewer(by, rv);
				rv.state = req.failed ? ReviewerState.FAILED : ReviewerState.DONE;
				rv.summary = req.summary;
				rv.ended = stamp;

				const stillRunning = rev.reviewers.filter((x) => x.state === ReviewerState.IN_PROGRESS);
				const anyDone = rev.reviewers.some((x) => x.state === ReviewerState.DONE);
				if (stillRunning.length > 0) {
					rev.state = ReviewState.IN_PROGRESS;
					rev.ended = undefined;
					const verb = req.failed ? "failed" : "finished";
					let line = `review ${rev.id} (${rev.title}): ${rv.model || rv.session} ${verb} (${stillRunning.length} still in progress)`;
					if (req.summary) line += `: ${firstLine(req.summary)}`;
					return {
						reviewerId: rv.id,
						stamp,
						notify: {
							what: req.failed ? ReviewUpdate_What.FAILED : ReviewUpdate_What.FINISHED,
							detail: line,
						},
					};
				}
				rev.state = anyDone ? ReviewState.DONE : ReviewState.FAILED;
				rev.summary = req.summary;
				rev.ended = stamp;
				const word = rev.state === ReviewState.DONE ? "is done" : "failed";
				let line = `review ${rev.id} (${rev.title}) ${word}`;
				if (rev.summary) line += `: ${firstLine(rev.summary)}`;
				return {
					reviewerId: rv.id,
					stamp,
					notify: {
						what:
							rev.state === ReviewState.DONE
								? ReviewUpdate_What.FINISHED
								: ReviewUpdate_What.FAILED,
						detail: line,
					},
				};
			});
			if (r.requested?.session) ctrl.sessionWakes.wake(r.requested.session);
			return ctrl.store.reviews.get(req.id, true);
		},

		cancelReview(req, ctx) {
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, req.reason, {
				model: req.model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});
			ctrl.store.reviews.change(req.id, "cancelled", by, (r) => {
				inProject("review", r.id, r.project, project, by);
				if (isReviewEnded(r.state)) {
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} is ${enumWord(ReviewStateSchema, r.state)} already, ${stamped(r.ended)}`,
					);
				}
				const stamp = enrichImplementerStamp(r, by);
				r.state = ReviewState.CANCELLED;
				r.summary = req.reason;
				r.ended = stamp;
				return { stamp };
			});
			return ctrl.store.reviews.get(req.id, true);
		},

		decideFindings(req, ctx) {
			const whatMap: Partial<Record<FindingDecision, string>> = {
				[FindingDecision.ACCEPTED]: "accepted",
				[FindingDecision.DISMISSED]: "dismissed",
				[FindingDecision.WONT_FIX]: "wont_fix",
				[FindingDecision.FIXED]: "fixed",
				[FindingDecision.INVALID]: "invalid",
				[FindingDecision.OPEN]: "reopened",
			};
			const what = whatMap[req.decision];
			if (!what) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"decide a finding accepted, dismissed, wont_fix, fixed, invalid or open",
				);
			}
			if (req.findingIds.length === 0) {
				throw hotbedError(ErrorCode.InvalidArgs, "name the findings").withSuggestion(
					`hotbed review show ${req.reviewId}`,
				);
			}
			checkDecision(req);
			const caller = getCaller(ctx.values);
			const { by, project } =
				req.actorKind === ActorKind.USER
					? ctrl.makeStamp(caller, {
							sessionId: req.sessionId,
							actorKind: ActorKind.USER,
							reason: req.reason,
						})
					: ctrl.agentStamp(caller, req.sessionId, req.reason, {
							model: req.model,
							subagent: req.subagent,
							t3codeThreadId: req.t3codeThreadId,
						});
			const findings = ctrl.store.reviews.changeFindings(
				req.reviewId,
				req.findingIds,
				what,
				by,
				(r, f) => {
					inProject("review", r.id, r.project, project, by);
					const stamp = enrichImplementerStamp(r, by);
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
						(f.decision === FindingDecision.DISMISSED ||
							f.decision === FindingDecision.INVALID ||
							f.decision === FindingDecision.WONT_FIX)
					) {
						throw hotbedError(
							ErrorCode.Conflict,
							`finding ${f.id} is ${decisionWord(f.decision)}: reopen it before you mark it fixed`,
						).withSuggestion(`hotbed review reopen ${r.id} ${f.id}`);
					}
					f.decision = req.decision;
					f.decided = req.decision === FindingDecision.OPEN ? undefined : stamp;
					f.fix = req.fix;
					f.evidence = req.evidence;
					return stamp;
				},
			);
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
			const { by, project } = ctrl.makeStamp(getCaller(ctx.values), {
				sessionId: req.sessionId,
				actorKind: req.actorKind,
				reason: "",
				model: req.model,
				subagent: req.subagent,
				t3codeThreadId: req.t3codeThreadId,
			});
			const ev = create(ReviewEventSchema, {
				reviewId: req.reviewId,
				findingId: req.findingId,
				by,
				quick: req.quick,
				text: req.text,
			});
			const saved = ctrl.store.reviews.addResponse(ev, req.after, (r, f) => {
				inProject("review", r.id, r.project, project, by);
				if (f?.decision === FindingDecision.FIXED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`finding ${f.id} is fixed, in ${f.fix?.commit ?? ""}: it is handled, and takes no more responses`,
					).withSuggestion(
						`hotbed review reopen ${r.id} ${f.id}, if the fix did not hold; or respond to the review as a whole`,
					);
				}
				ev.by = enrichImplementerStamp(r, by);
			});
			const r = ctrl.store.reviews.get(req.reviewId, false);
			if (r.requested?.session) ctrl.sessionWakes.wake(r.requested.session);
			return saved;
		},

		listResponses(req) {
			ctrl.store.reviews.get(req.reviewId, false);
			const events = ctrl.store.reviews.eventsAfter(req.reviewId, req.after);
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
