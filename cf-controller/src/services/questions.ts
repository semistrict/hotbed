import type { ServiceImpl } from "@connectrpc/connect";
import { clone, create } from "@bufbuild/protobuf";
import {
	AckQuestionsResponseSchema,
	AddQuestionsResponseSchema,
	AnswerSchema,
	ListQuestionProjectsResponseSchema,
	ListQuestionsResponseSchema,
	QuestionSchema,
	QuestionService,
	QuestionState,
	type Answer,
	type Project,
	type Prompt,
	type Question,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { getCaller } from "../connect-adapter.ts";
import { inProject, stamped, validateT3CodeThreadId, type ControllerContext } from "../context.ts";
import { ErrorCode, hotbedError, type HotbedError } from "../errors.ts";
import { canonicalRemote } from "../gitops.ts";

const MAX_ASKED = 4;
const MAX_HEADER = 12;
const MIN_OPTIONS = 2;
const MAX_OPTIONS = 4;
const DIRTY_KEPT = 20;

const POLL_REMINDER =
	"Reminder: the user answers questions in the cockpit whenever they like, and answers reach you only through `hotbed poll` (MCP: `poll`). " +
	"Before you end your turn, commit, or conclude the user has not answered, run `hotbed poll`; " +
	"act on each answer it returns, then `hotbed question ack ID`.";

function withReminder(q: Question): Question {
	const out = clone(QuestionSchema, q);
	out.reminder = POLL_REMINDER;
	return out;
}

function inOutbox(st: QuestionState): boolean {
	return st === QuestionState.ANSWERED || st === QuestionState.DISMISSED;
}

function stateWord(st: QuestionState): string {
	switch (st) {
		case QuestionState.OPEN:
			return "open";
		case QuestionState.ANSWERED:
			return "answered";
		case QuestionState.DISMISSED:
			return "dismissed";
		case QuestionState.HANDLED:
			return "handled";
		case QuestionState.DELETED:
			return "deleted";
		default:
			return "unspecified";
	}
}

function checkPrompt(p?: Prompt): HotbedError | null {
	if (!p || !p.question.trim()) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			"a question needs its question, a short title; put the detail in its body",
		);
	}
	if (!p.header.trim()) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			`question "${p.question}" needs a header, a short label for it`,
		).withSuggestion(`"header": "Auth method"`);
	}
	if (Array.from(p.header).length > MAX_HEADER) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			`header "${p.header}" is longer than ${MAX_HEADER} characters`,
		);
	}
	if (p.options.length < MIN_OPTIONS || p.options.length > MAX_OPTIONS) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			`question "${p.question}" has ${p.options.length} options; give ${MIN_OPTIONS} to ${MAX_OPTIONS} (the user can always write their own answer)`,
		);
	}
	const labels: string[] = [];
	const implemented: string[] = [];
	const recommended: string[] = [];
	for (const o of p.options) {
		const label = o.label.trim();
		if (!label) {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`question "${p.question}" has an option with no label`,
			);
		}
		if (!o.description.trim()) {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`option "${o.label}" needs a description: what picking it means`,
			);
		}
		if (label.toLowerCase() === "other") {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`leave out the "${o.label}" option: the user can always write their own answer`,
			);
		}
		if (labels.includes(label)) {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`question "${p.question}" has two options labelled "${o.label}"`,
			);
		}
		if (o.implemented && o.recommended) {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`option "${o.label}" is both implemented and recommended: recommended is for a way forward not built yet`,
			).withSuggestion(
				`keep "implemented": true, and mark another option recommended only if you would change to it`,
			);
		}
		labels.push(label);
		if (o.implemented) implemented.push(o.label);
		if (o.recommended) recommended.push(o.label);
	}
	if (!p.multiSelect) {
		for (const [mark, list] of [
			["implemented", implemented],
			["recommended", recommended],
		] as const) {
			if (list.length > 1) {
				return hotbedError(
					ErrorCode.InvalidArgs,
					`question "${p.question}" takes one option, but ${list.length} are ${mark}: ${list.join(", ")}`,
				).withSuggestion(`mark one, or set "multiSelect": true if several go together`);
			}
		}
	}
	return null;
}

function checkAnswer(q: Question, a?: Answer): HotbedError | null {
	if (!a || (a.picked.length === 0 && !a.other.trim())) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			"an answer picks an option or says something of its own",
		);
	}
	if (a.picked.length > 1 && !q.prompt?.multiSelect) {
		return hotbedError(
			ErrorCode.InvalidArgs,
			`question ${q.id} takes one option; ${a.picked.length} were picked`,
		);
	}
	const labels = (q.prompt?.options ?? []).map((o) => o.label);
	for (let i = 0; i < a.picked.length; i++) {
		const p = a.picked[i];
		if (!labels.includes(p)) {
			return hotbedError(
				ErrorCode.InvalidArgs,
				`question ${q.id} has no option "${p}"`,
			).withSuggestion(`its options: ${labels.join(", ")}`);
		}
		if (a.picked.indexOf(p) !== i) {
			return hotbedError(ErrorCode.InvalidArgs, `option "${p}" was picked twice`);
		}
	}
	return null;
}

function decisionFor(p: Prompt): { answer?: Answer; err?: HotbedError } {
	const a = create(AnswerSchema, {
		picked: p.options.filter((o) => o.implemented).map((o) => o.label),
	});
	if (a.picked.length === 0) {
		return {
			err: hotbedError(
				ErrorCode.InvalidArgs,
				`question "${p.question}" is answered, but no option is implemented: mark what was decided`,
			).withSuggestion(`{"label": "What was decided", "description": "...", "implemented": true}`),
		};
	}
	const err = checkAnswer(create(QuestionSchema, { prompt: p }), a);
	return err ? { err } : { answer: a };
}

function questionRepo(project: Project, named: string, askedFrom: string): string {
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
	const from = canonicalRemote(askedFrom);
	if (from && project.repos.includes(from)) return from;
	return "";
}

function checkRevision(q: Question, want: bigint): void {
	if (want > 0n && q.revision !== want) {
		throw hotbedError(
			ErrorCode.Conflict,
			`question ${q.id} is at revision ${q.revision}, not ${want}`,
		).withSuggestion(`hotbed question show ${q.id}`);
	}
}

function mustBeOpen(q: Question, done: string): void {
	if (q.state !== QuestionState.OPEN) {
		throw hotbedError(
			ErrorCode.Conflict,
			`question ${q.id} is ${stateWord(q.state)}; only an open question can be ${done}`,
		).withSuggestion(
			`hotbed question show ${q.id} says who changed it; ask a new question if it still needs asking`,
		);
	}
}

function mustBeAnswerable(q: Question, done: string): void {
	if (q.state !== QuestionState.OPEN && !inOutbox(q.state)) {
		throw hotbedError(
			ErrorCode.Conflict,
			`question ${q.id} is ${stateWord(q.state)}; only an open question, or one in the outbox, can be ${done}`,
		).withSuggestion(`reopen it first: hotbed question reopen ${q.id}`);
	}
}

export function createQuestionService(
	ctrl: ControllerContext,
): ServiceImpl<typeof QuestionService> {
	return {
		addQuestions(req, ctx) {
			const caller = getCaller(ctx.values);
			if (req.questions.length === 0 || req.questions.length > MAX_ASKED) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`ask one to ${MAX_ASKED} questions at a time, as AskUserQuestion does; got ${req.questions.length}`,
				).withSuggestion(
					`{"questions": [{"question": "Which ...?", "header": "Short", "options": [{"label": "A", "description": "..."}, {"label": "B", "description": "..."}], "multiSelect": false}]}`,
				);
			}
			const decided: Answer[] = [];
			for (let i = 0; i < req.questions.length; i++) {
				const p = req.questions[i];
				let err = checkPrompt(p);
				if (!err && req.answered) {
					const d = decisionFor(p);
					if (d.err) err = d.err;
					else if (d.answer) decided.push(d.answer);
				}
				if (err) {
					throw hotbedError(
						ErrorCode.InvalidArgs,
						`question ${i + 1} of ${req.questions.length}: ${err.message}`,
						{ suggestion: err.suggestion },
					);
				}
			}
			const t3Thread = validateT3CodeThreadId(req.t3codeThreadId);
			const { by, project } = ctrl.agentStamp(caller, req.sessionId, "");
			const sess = ctrl.store.tryGetSession(by.session);
			if (t3Thread && sess && !sess.t3codeThreadId) {
				ctrl.store.setSessionT3CodeThreadIdIfEmpty(sess.id, t3Thread);
			}
			const effectiveT3 = t3Thread || sess?.t3codeThreadId || "";
			const repo = questionRepo(project, req.repo, req.askedFrom?.remote ?? "");
			if (req.askedFrom && req.askedFrom.dirty.length > DIRTY_KEPT) {
				req.askedFrom.dirty = req.askedFrom.dirty.slice(0, DIRTY_KEPT);
			}
			const qs = ctrl.store.addQuestions(
				project.name,
				repo,
				req.questions,
				req.answered ? decided : null,
				by,
				req.askedFrom,
				effectiveT3,
			);
			return create(AddQuestionsResponseSchema, { questions: qs, reminder: POLL_REMINDER });
		},

		editQuestion(req, ctx) {
			const err = checkPrompt(req.prompt);
			if (err) throw err;
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			return withReminder(
				ctrl.store.changeQuestion(req.id, "edited", by, (q) => {
					inProject("question", q.id, q.project, project, by);
					checkRevision(q, req.ifRevision);
					mustBeOpen(q, "edited");
					q.prompt = req.prompt;
					q.edited = by;
				}),
			);
		},

		deleteQuestion(req, ctx) {
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, req.reason);
			const q = ctrl.store.changeQuestion(req.id, "deleted", by, (item) => {
				inProject("question", item.id, item.project, project, by);
				checkRevision(item, req.ifRevision);
				if (item.state === QuestionState.DELETED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`question ${item.id} was deleted already, ${stamped(item.deleted)}`,
					);
				}
				item.state = QuestionState.DELETED;
				item.deleted = by;
			});
			ctrl.questionBridge?.broadcastUpdate("deleted", [q]);
			return withReminder(q);
		},

		listQuestions(req) {
			if (req.limit < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `limit ${req.limit}: give 0 for all, or how many`);
			}
			const tid = validateT3CodeThreadId(req.t3codeThreadId);
			let project = "";
			if (
				!tid ||
				req.scope.case === "allProjects" ||
				req.scope.case === "project" ||
				req.sessionId ||
				req.remote
			) {
				project = ctrl.listedProject(req);
			}
			const { questions, total, sessions } = ctrl.store.listQuestions({
				states: req.states,
				order: req.order,
				project,
				askedBy: req.askedBy,
				t3codeThreadId: tid,
				limit: req.limit,
			});
			return create(ListQuestionsResponseSchema, {
				questions,
				total,
				sessions,
				reminder: POLL_REMINDER,
			});
		},

		listQuestionProjects() {
			return create(ListQuestionProjectsResponseSchema, {
				projects: ctrl.store.questionProjects(),
				reminder: POLL_REMINDER,
			});
		},

		getQuestion(req) {
			const q = ctrl.store.getQuestion(req.id);
			q.history = ctrl.store.questionHistory(req.id);
			return withReminder(q);
		},

		answerQuestion(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const q = ctrl.store.changeQuestion(req.id, "answered", by, (item) => {
				inProject("question", item.id, item.project, project, by);
				checkRevision(item, req.ifRevision);
				mustBeAnswerable(item, "answered");
				const err = checkAnswer(item, req.answer);
				if (err) throw err;
				item.state = QuestionState.ANSWERED;
				item.answer = req.answer;
				item.answered = by;
				item.dismissed = undefined;
			});
			if (q.asked?.session) ctrl.sessionWakes.wake(q.asked.session);
			ctrl.questionBridge?.broadcastUpdate("answered", [q]);
			return withReminder(q);
		},

		dismissQuestion(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, req.reason);
			const q = ctrl.store.changeQuestion(req.id, "dismissed", by, (item) => {
				inProject("question", item.id, item.project, project, by);
				checkRevision(item, req.ifRevision);
				mustBeAnswerable(item, "dismissed");
				item.state = QuestionState.DISMISSED;
				item.dismissed = by;
				item.answer = undefined;
				item.answered = undefined;
			});
			if (q.asked?.session) ctrl.sessionWakes.wake(q.asked.session);
			ctrl.questionBridge?.broadcastUpdate("dismissed", [q]);
			return withReminder(q);
		},

		reviseQuestion(req, ctx) {
			const err = checkPrompt(req.prompt);
			if (err) throw err;
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const q = ctrl.store.changeQuestion(req.id, "edited", by, (item) => {
				inProject("question", item.id, item.project, project, by);
				checkRevision(item, req.ifRevision);
				if (item.state === QuestionState.DELETED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`question ${item.id} was deleted ${stamped(item.deleted)}; ask it again if it still needs asking`,
					);
				}
				const revised = clone(QuestionSchema, item);
				revised.prompt = req.prompt;
				if (item.answer) {
					const aerr = checkAnswer(revised, item.answer);
					if (aerr) {
						throw hotbedError(
							ErrorCode.Conflict,
							`question ${item.id}'s answer no longer fits it: ${aerr.message}`,
						).withSuggestion(
							`keep the options it picks, or reopen it first: hotbed question reopen ${item.id}`,
						);
					}
				}
				item.prompt = req.prompt;
				item.edited = by;
			});
			ctrl.questionBridge?.broadcastUpdate("revised", [q]);
			return withReminder(q);
		},

		reopenQuestion(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			const q = ctrl.store.reopenQuestion(req.id, by, (item) => {
				inProject("question", item.id, item.project, project, by);
				if (item.state === QuestionState.OPEN) {
					throw hotbedError(ErrorCode.Conflict, `question ${item.id} is open already`);
				}
				if (item.state === QuestionState.DELETED) {
					throw hotbedError(
						ErrorCode.Conflict,
						`question ${item.id} was deleted ${stamped(item.deleted)}; ask it again if it still needs asking`,
					);
				}
				item.state = QuestionState.OPEN;
				item.answer = undefined;
				item.answered = undefined;
				item.dismissed = undefined;
				item.handled = undefined;
			});
			ctrl.questionBridge?.broadcastUpdate("reopened", [q]);
			return withReminder(q);
		},

		moveQuestion(req, ctx) {
			const { by, project } = ctrl.userStamp(getCaller(ctx.values), req.sessionId, "");
			let place: { before?: number; first?: boolean } = {};
			if (req.place.case === "before") {
				if (req.place.value === req.id) {
					throw hotbedError(ErrorCode.InvalidArgs, `question ${req.id} cannot go before itself`);
				}
				place = { before: req.place.value };
			} else if (req.place.case === "first") {
				place = { first: req.place.value };
			} else if (req.place.case !== "last" || !req.place.value) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					`say where question ${req.id} goes: before another, first or last`,
				);
			}
			return withReminder(
				ctrl.store.moveQuestion(req.id, place, by, (q, before) => {
					inProject("question", q.id, q.project, project, by);
					mustBeOpen(q, "moved");
					if (before && before.project !== q.project) {
						throw hotbedError(
							ErrorCode.InvalidArgs,
							`question ${before.id} is in project ${before.project}, not ${q.project}: move questions within their own project`,
						);
					}
					if (before && before.state !== QuestionState.OPEN) {
						throw hotbedError(
							ErrorCode.Conflict,
							`question ${before.id} is ${stateWord(before.state)}: only open questions have a place in the order`,
						);
					}
				}),
			);
		},

		ackQuestions(req, ctx) {
			if (req.ids.length === 0) {
				throw hotbedError(
					ErrorCode.InvalidArgs,
					"name the questions whose answers you read",
				).withSuggestion("hotbed question outbox");
			}
			const { by, project } = ctrl.agentStamp(getCaller(ctx.values), req.sessionId, "");
			const qs = ctrl.store.changeQuestions(req.ids, "handled", by, (q) => {
				inProject("question", q.id, q.project, project, by);
				if (!inOutbox(q.state)) {
					throw hotbedError(
						ErrorCode.Conflict,
						`question ${q.id} is ${stateWord(q.state)}, not in the outbox: nothing was acknowledged`,
					).withSuggestion("hotbed question outbox lists what can be acknowledged");
				}
				q.state = QuestionState.HANDLED;
				q.handled = by;
			});
			ctrl.questionBridge?.broadcastUpdate("handled", qs);
			return create(AckQuestionsResponseSchema, {
				questions: qs,
				reminder: `${POLL_REMINDER} If the user's answer reflects a standing preference or project convention, save it as a memory (\`hotbed memory add --slug SLUG --summary "..."\`, MCP: \`memory_add\`).`,
			});
		},
	};
}
