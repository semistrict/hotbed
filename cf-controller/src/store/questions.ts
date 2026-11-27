import { clone, create } from "@bufbuild/protobuf";
import { enumName } from "../enums.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import {
	QuestionChangeSchema,
	QuestionOrder,
	QuestionProjectSchema,
	QuestionSchema,
	QuestionSessionSchema,
	QuestionState,
	QuestionStateSchema,
	QuestionUpdate_What,
	QuestionUpdateSchema,
	SessionEventSchema,
	type Answer,
	type AskedFrom,
	type Prompt,
	type Question,
	type QuestionChange,
	type QuestionProject,
	type QuestionSession,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import {
	appendAuditChange,
	auditChanges,
	marshal,
	moveRankedItem,
	topRank,
	unmarshal,
	type RankedList,
	type Row,
	type SqlKernel,
} from "./kernel.ts";
import type { ProjectStore } from "./projects.ts";
import type { SessionStore } from "./sessions.ts";

const stateName = (s: QuestionState): string => enumName(QuestionStateSchema, s);

const TRAIL = { table: "question_changes", key: "question_id" };

/**
 * A question's columns, with the T3 Code thread it was asked from: its own,
 * else its asking session's.
 */
const QUESTION_SELECT = `SELECT q.id, q.rank, p.name AS project, q.doc,
	        q.t3code_thread_id AS q_t3code_thread_id,
	        COALESCE(s.t3code_thread_id, '') AS s_t3code_thread_id
	 FROM questions q
	 JOIN projects p ON p.id = q.project_id
	 LEFT JOIN sessions s ON s.id = q.asked_by`;

function questionRow(r: Row): Question {
	const q = unmarshal(QuestionSchema, str(r.doc));
	q.id = num(r.id);
	q.rank = big(r.rank);
	q.project = str(r.project);
	q.t3codeThreadId = str(r.q_t3code_thread_id) || str(r.s_t3code_thread_id) || q.t3codeThreadId;
	return q;
}

function questionResponseLine(q: Question): string {
	const head = `question ${q.id} (${q.prompt?.header ?? ""})`;
	if (q.state === QuestionState.DISMISSED) {
		return q.dismissed?.reason ? `${head} dismissed: ${q.dismissed.reason}` : `${head} dismissed`;
	}
	const parts = [...(q.answer?.picked ?? [])];
	if (q.answer?.other) parts.push(q.answer.other);
	return `${head} answered: ${parts.join("; ")}`;
}

/** Questions for the user: hand-ranked, with an audit trail, telling the asking session when answered. */
export class QuestionStore {
	private readonly ranked: RankedList<Question>;

	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
		private readonly sessions: SessionStore,
	) {
		// One ranking across every project's questions; `first` goes above
		// the top open question of the item's own project.
		this.ranked = {
			table: "questions",
			scope: "1",
			scopeArgs: [],
			get: (id) => this.get(id),
			topOf: (item) => {
				const r = this.sql.first(
					`SELECT q.id FROM questions q JOIN projects p ON p.id = q.project_id
					 WHERE p.name = ? AND q.state = ? ORDER BY q.rank, q.id LIMIT 1`,
					item.project,
					stateName(QuestionState.OPEN),
				);
				return r ? num(r.id) : undefined;
			},
			change: (id, what, by, edit) => this.changeTx(id, what, by, edit),
		};
	}

	add(
		project: string,
		repo: string,
		prompts: Prompt[],
		decided: Answer[] | null,
		by: Stamp,
		from?: AskedFrom,
		t3codeThreadId = "",
	): Question[] {
		return this.sql.tx(() => {
			const pid = this.projects.idByName(project);
			const top = topRank(this.sql, this.ranked);
			const out: Question[] = [];
			for (let i = 0; i < prompts.length; i++) {
				const rank = BigInt(top - (prompts.length - i));
				const item = create(QuestionSchema, {
					prompt: prompts[i],
					state: QuestionState.OPEN,
					rank,
					asked: by,
					askedFrom: from,
					updatedAt: by.at,
					project,
					repo,
					t3codeThreadId,
				});
				item.id = this.sql.insert(
					"INSERT INTO questions (project_id, repo, state, rank, asked_by, t3code_thread_id, asked_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '')",
					pid,
					repo,
					stateName(item.state),
					rank,
					by.session,
					t3codeThreadId,
					by.at,
					by.at,
				);
				this.save(item, "asked", by);
				if (decided) {
					item.state = QuestionState.ANSWERED;
					item.answer = decided[i];
					item.answered = by;
					this.save(item, "answered", by);
					item.state = QuestionState.HANDLED;
					item.handled = by;
					this.save(item, "handled", by);
				}
				out.push(item);
			}
			return out;
		});
	}

	get(id: number): Question {
		const r = this.sql.first(`${QUESTION_SELECT} WHERE q.id = ?`, id);
		if (!r) throw hotbedError(ErrorCode.NotFound, `no question ${id}`);
		return questionRow(r);
	}

	history(id: number): QuestionChange[] {
		return auditChanges(this.sql, TRAIL, id).map((c) =>
			create(QuestionChangeSchema, {
				seq: c.seq,
				what: c.what,
				by: c.by,
				question: unmarshal(QuestionSchema, c.doc),
			}),
		);
	}

	list(filter: {
		states: QuestionState[];
		order: QuestionOrder;
		project: string;
		askedBy: string;
		t3codeThreadId: string;
		limit: number;
	}): { questions: Question[]; total: number; sessions: QuestionSession[] } {
		if (filter.project) {
			this.projects.get(filter.project);
		}
		const states =
			filter.states.length > 0
				? filter.states.map(stateName)
				: [
						QuestionState.OPEN,
						QuestionState.ANSWERED,
						QuestionState.DISMISSED,
						QuestionState.HANDLED,
					].map(stateName);
		const where = `WHERE q.state IN (${states.map(() => "?").join(",")})
			   AND (? = '' OR p.name = ?)
			   AND (? = '' OR q.asked_by = ?)
			   AND (? = '' OR COALESCE(NULLIF(q.t3code_thread_id, ''), COALESCE(s.t3code_thread_id, '')) = ?)`;
		const args = [
			...states,
			filter.project,
			filter.project,
			filter.askedBy,
			filter.askedBy,
			filter.t3codeThreadId,
			filter.t3codeThreadId,
		];
		const total = num(
			this.sql
				.exec(
					`SELECT COUNT(*) AS total
					 FROM questions q
					 JOIN projects p ON p.id = q.project_id
					 LEFT JOIN sessions s ON s.id = q.asked_by
					 ${where}`,
					...args,
				)
				.one().total,
		);
		const orderBy =
			filter.order === QuestionOrder.OLDEST
				? "q.asked_at ASC, q.id ASC"
				: filter.order === QuestionOrder.UPDATED
					? "q.updated_at DESC, q.id DESC"
					: filter.order === QuestionOrder.RANK
						? "q.rank ASC, q.id ASC"
						: "q.asked_at DESC, q.id DESC";
		const sessionsById = new Map<string, QuestionSession>();
		const sessions: QuestionSession[] = [];
		const questions = this.sql
			.rows(
				`SELECT q.id, q.rank, p.name AS project, q.doc,
				        q.t3code_thread_id AS q_t3code_thread_id,
				        COALESCE(s.t3code_thread_id, '') AS s_t3code_thread_id,
				        (s.id IS NOT NULL AND s.ended_at IS NULL) AS s_live
				 FROM questions q
				 JOIN projects p ON p.id = q.project_id
				 LEFT JOIN sessions s ON s.id = q.asked_by
				 ${where}
				 ORDER BY ${orderBy} LIMIT ?`,
				...args,
				filter.limit > 0 ? filter.limit : -1,
			)
			.map((r) => {
				const q = questionRow(r);
				const sid = q.asked?.session ?? "";
				if (sid) {
					let entry = sessionsById.get(sid);
					if (!entry) {
						entry = create(QuestionSessionSchema, {
							sessionId: sid,
							project: q.project,
							t3codeThreadId: q.t3codeThreadId,
							live: num(r.s_live) !== 0,
							questionIds: [],
						});
						sessionsById.set(sid, entry);
						sessions.push(entry);
					} else if (!entry.t3codeThreadId && q.t3codeThreadId) {
						entry.t3codeThreadId = q.t3codeThreadId;
					}
					entry.questionIds.push(q.id);
					if (q.state === QuestionState.OPEN) entry.open += 1;
					else if (q.state === QuestionState.ANSWERED) entry.answered += 1;
					else if (q.state === QuestionState.DISMISSED) entry.dismissed += 1;
				}
				return q;
			});
		return { questions, total, sessions };
	}

	/** Answered and dismissed questions asked from a T3 Code thread, oldest change first: what the question bridge delivers. */
	t3Outbox(): Question[] {
		return this.sql
			.rows(
				`${QUESTION_SELECT}
				 WHERE q.state IN (?, ?)
				   AND COALESCE(NULLIF(q.t3code_thread_id, ''), COALESCE(s.t3code_thread_id, '')) != ''
				 ORDER BY q.updated_at ASC, q.id ASC`,
				stateName(QuestionState.ANSWERED),
				stateName(QuestionState.DISMISSED),
			)
			.map(questionRow);
	}

	/** Each project's open, outbox and total questions. */
	byProject(): QuestionProject[] {
		return this.sql
			.rows(
				`SELECT p.name,
				        SUM(q.state = ?) AS open_count,
				        SUM(q.state IN (?, ?)) AS outbox_count,
				        COUNT(*) AS total
				 FROM questions q JOIN projects p ON p.id = q.project_id
				 GROUP BY p.name ORDER BY p.name`,
				stateName(QuestionState.OPEN),
				stateName(QuestionState.ANSWERED),
				stateName(QuestionState.DISMISSED),
			)
			.map((r) =>
				create(QuestionProjectSchema, {
					name: str(r.name),
					open: num(r.open_count),
					outbox: num(r.outbox_count),
					total: num(r.total),
				}),
			);
	}

	openCount(): number {
		return num(
			this.sql
				.exec(
					"SELECT COUNT(*) AS cnt FROM questions WHERE state = ?",
					stateName(QuestionState.OPEN),
				)
				.one().cnt,
		);
	}

	change(id: number, what: string, by: Stamp, edit: (q: Question) => void): Question {
		return this.sql.tx(() => this.changeTx(id, what, by, edit));
	}

	changeMany(ids: number[], what: string, by: Stamp, edit: (q: Question) => void): Question[] {
		return this.sql.tx(() => ids.map((id) => this.changeTx(id, what, by, edit)));
	}

	/** Reopens id on top of the ranking. */
	reopen(id: number, by: Stamp, edit: (q: Question) => void): Question {
		return this.sql.tx(() => {
			const top = topRank(this.sql, this.ranked);
			return this.changeTx(id, "reopened", by, (item) => {
				edit(item);
				item.rank = BigInt(top - 1);
			});
		});
	}

	move(
		id: number,
		place: { before?: number; first?: boolean },
		by: Stamp,
		check: (q: Question, before: Question | null) => void,
	): Question {
		return moveRankedItem(this.sql, this.ranked, id, place, by, check);
	}

	private changeTx(id: number, what: string, by: Stamp, edit: (q: Question) => void): Question {
		const item = this.get(id);
		edit(item);
		if (what !== "moved") item.updatedAt = by.at;
		this.save(item, what, by);
		if (what === "answered" || what === "dismissed") {
			const stamp = what === "dismissed" ? item.dismissed : item.answered;
			const w = what === "dismissed" ? QuestionUpdate_What.DISMISSED : QuestionUpdate_What.ANSWERED;
			const copy = clone(QuestionSchema, item);
			copy.history = [];
			this.sessions.tell(
				item.asked?.session ?? "",
				stamp?.at ?? by.at,
				create(SessionEventSchema, {
					detail: questionResponseLine(item),
					event: {
						case: "question",
						value: create(QuestionUpdateSchema, { what: w, question: copy }),
					},
				}),
			);
		}
		return item;
	}

	/** Saves item (rank and project live in their own columns) and appends it to its trail. */
	private save(item: Question, what: string, by: Stamp): void {
		item.revision += 1n;
		const stored = clone(QuestionSchema, item);
		stored.rank = 0n;
		stored.history = [];
		stored.project = "";
		this.sql.exec(
			"UPDATE questions SET state = ?, rank = ?, updated_at = ?, doc = ? WHERE id = ?",
			stateName(item.state),
			item.rank,
			item.updatedAt,
			marshal(QuestionSchema, stored),
			item.id,
		);
		stored.rank = item.rank;
		stored.project = item.project;
		appendAuditChange(this.sql, TRAIL, item.id, what, by, marshal(QuestionSchema, stored));
	}
}
