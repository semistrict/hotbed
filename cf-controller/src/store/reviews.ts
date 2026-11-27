import {
	ActorKind,
	FindingDecision,
	FindingSchema,
	FindingSeverity,
	QuickResponse,
	QuickResponseSchema,
	ReviewEventSchema,
	ReviewerSchema,
	ReviewerState,
	ReviewSchema,
	ReviewState,
	ReviewStateSchema,
	ReviewUpdate_What,
	ReviewUpdateSchema,
	SessionEventSchema,
	StampSchema,
	type Finding,
	type Review,
	type ReviewEvent,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { enumFromName, enumName } from "../enums.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { firstLine } from "../encoding.ts";
import { create, clone } from "@bufbuild/protobuf";
import { marshal, unmarshal, type Row, type SqlKernel } from "./kernel.ts";
import type { ProjectStore } from "./projects.ts";
import type { SessionStore } from "./sessions.ts";

const stateName = (s: ReviewState): string => enumName(ReviewStateSchema, s);

function isUserStamp(by?: Stamp): boolean {
	if (!by) return true;
	if (by.actorKind === ActorKind.USER) return true;
	if (by.actorKind === ActorKind.AGENT) return false;
	return !by.session;
}

function reviewResponseOn(about: string, e: ReviewEvent): string {
	const who = isUserStamp(e.by) ? "the user" : e.by?.session || "an agent";
	const parts: string[] = [];
	if (e.quick !== QuickResponse.UNSPECIFIED) {
		const word =
			e.quick === QuickResponse.VERIFY
				? "verify"
				: e.quick === QuickResponse.FIX_IF_VERIFIED
					? "fix if verified"
					: "won't fix";
		parts.push(word);
	}
	if (e.text) parts.push(JSON.stringify(firstLine(e.text)));
	return `${who} on ${about}: ${parts.join("; ")}`;
}

/** Code reviews, their findings and their event log, telling the requesting session of changes. */
export class ReviewStore {
	constructor(
		private readonly sql: SqlKernel,
		private readonly projects: ProjectStore,
		private readonly sessions: SessionStore,
	) {}

	add(project: string, r: Review): Review {
		return this.sql.tx(() => {
			const pid = this.projects.idByName(project);
			const reqAt = r.requested?.at ?? 0n;
			r.id = this.sql.insert(
				"INSERT INTO reviews (project_id, state, requested_by, requested_at, updated_at, doc) VALUES (?, ?, ?, ?, ?, '')",
				pid,
				stateName(r.state),
				r.requested?.session ?? "",
				reqAt,
				reqAt,
			);
			r.project = project;
			r.updatedAt = reqAt;
			this.save(r, "requested", r.requested!);
			return r;
		});
	}

	private tellRequester(
		session: string,
		at: bigint,
		what: ReviewUpdate_What,
		reviewId: number,
		detail: string,
	): void {
		this.sessions.tell(
			session,
			at,
			create(SessionEventSchema, {
				detail,
				event: { case: "review", value: create(ReviewUpdateSchema, { what, reviewId }) },
			}),
		);
	}

	private normalizeLegacy(r: Review): void {
		if (r.reviewers.length === 0 && r.started) {
			const state =
				r.state === ReviewState.DONE
					? ReviewerState.DONE
					: r.state === ReviewState.FAILED
						? ReviewerState.FAILED
						: ReviewerState.IN_PROGRESS;
			r.reviewers.push(
				create(ReviewerSchema, {
					id: 1,
					model: r.model,
					session: r.started.session,
					t3codeThreadId: r.started.t3codeThreadId,
					state,
					started: r.started,
					ended: r.ended,
					summary: r.summary,
					report: r.report,
				}),
			);
		}
	}

	get(id: number, withDetail: boolean): Review {
		const rows = this.sql
			.exec(
				"SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id WHERE r.id = ?",
				id,
			)
			.toArray();
		if (rows.length === 0) throw hotbedError(ErrorCode.NotFound, `no review ${id}`);
		const r = unmarshal(ReviewSchema, str(rows[0].doc));
		r.id = num(rows[0].id);
		r.project = str(rows[0].project);
		this.normalizeLegacy(r);
		this.attachCountsAndDetails(r, withDetail);
		return r;
	}

	list(project: string, states: ReviewState[], limit: number): Review[] {
		const lim = limit > 0 ? limit : -1;
		let rows: Array<Row>;
		if (states.length > 0) {
			const names = states.map(stateName);
			const ph = names.map(() => "?").join(",");
			rows = this.sql
				.exec(
					`SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id
					 WHERE (? = '' OR p.name = ?) AND r.state IN (${ph})
					 ORDER BY r.requested_at DESC, r.id DESC LIMIT ?`,
					project,
					project,
					...names,
					lim,
				)
				.toArray();
		} else {
			rows = this.sql
				.exec(
					`SELECT r.id, p.name AS project, r.doc FROM reviews r JOIN projects p ON p.id = r.project_id
					 WHERE (? = '' OR p.name = ?)
					 ORDER BY r.requested_at DESC, r.id DESC LIMIT ?`,
					project,
					project,
					lim,
				)
				.toArray();
		}
		return rows.map((row) => {
			const r = unmarshal(ReviewSchema, str(row.doc));
			r.id = num(row.id);
			r.project = str(row.project);
			this.normalizeLegacy(r);
			r.report = "";
			for (const rv of r.reviewers) rv.report = "";
			this.attachCountsAndDetails(r, false);
			return r;
		});
	}

	counts(): { active: Map<string, number>; toDecide: Map<string, number> } {
		const active = new Map<string, number>();
		const toDecide = new Map<string, number>();
		for (const r of this.sql
			.exec(
				`SELECT p.name, COUNT(*) AS cnt FROM reviews r JOIN projects p ON p.id = r.project_id
				 WHERE r.state IN (?, ?) GROUP BY p.name`,
				stateName(ReviewState.REQUESTED),
				stateName(ReviewState.IN_PROGRESS),
			)
			.toArray()) {
			active.set(str(r.name), num(r.cnt));
		}
		for (const r of this.sql
			.exec(
				`SELECT p.name, COUNT(DISTINCT r.id) AS cnt
				 FROM reviews r
				 JOIN projects p ON p.id = r.project_id
				 JOIN review_findings f ON f.review_id = r.id
				 WHERE r.state = ?
				   AND COALESCE(json_extract(f.doc, '$.decision'), 'FINDING_DECISION_OPEN') IN ('FINDING_DECISION_OPEN', 'FINDING_DECISION_UNSPECIFIED')
				 GROUP BY p.name`,
				stateName(ReviewState.DONE),
			)
			.toArray()) {
			toDecide.set(str(r.name), num(r.cnt));
		}
		return { active, toDecide };
	}

	change(
		id: number,
		what: string,
		by: Stamp,
		edit: (r: Review) => {
			reviewerId?: number;
			stamp?: Stamp;
			notify?: { what: ReviewUpdate_What; detail: string } | null;
		} | void,
	): Review {
		return this.sql.tx(() => {
			const r = this.get(id, false);
			const res = edit(r) ?? {};
			const stamp = res.stamp ?? by;
			const reviewerId = res.reviewerId ?? 0;
			r.updatedAt = stamp.at;
			this.save(r, what, stamp, reviewerId);
			if (res.notify !== undefined) {
				if (res.notify) {
					this.tellRequester(
						r.requested?.session ?? "",
						stamp.at,
						res.notify.what,
						r.id,
						res.notify.detail,
					);
				}
			} else if (what === "finished" || what === "failed") {
				const word = what === "finished" ? "is done" : "failed";
				let line = `review ${r.id} (${r.title}) ${word}`;
				if (r.summary) line += `: ${firstLine(r.summary)}`;
				const w = what === "finished" ? ReviewUpdate_What.FINISHED : ReviewUpdate_What.FAILED;
				this.tellRequester(r.requested?.session ?? "", stamp.at, w, r.id, line);
			}
			return r;
		});
	}

	addFindings(
		reviewId: number,
		by: Stamp,
		check: (r: Review) => { reviewerId: number; stamp: Stamp },
		findings: Finding[],
		report: string,
	): Finding[] {
		return this.sql.tx(() => {
			const r = this.get(reviewId, false);
			const { reviewerId, stamp } = check(r);
			if (report) {
				r.report = report;
				const rv = r.reviewers.find((x) => x.id === reviewerId);
				if (rv) rv.report = report;
				r.updatedAt = stamp.at;
				this.save(r, "reported", stamp, reviewerId);
			}
			if (findings.length === 0) {
				this.insertEvent(
					create(ReviewEventSchema, { reviewId, what: "found none", by: stamp, reviewerId }),
					"",
				);
			}
			for (const f of findings) {
				f.id = this.sql.insert(
					"INSERT INTO review_findings (review_id, doc) VALUES (?, '')",
					reviewId,
				);
				f.reviewId = reviewId;
				f.reviewerId = reviewerId;
				f.found = stamp;
				f.decision = FindingDecision.OPEN;
				this.saveFinding(f, "found", stamp, reviewerId);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				stamp.at,
				reviewId,
			);
			return findings;
		});
	}

	changeFindings(
		reviewId: number,
		findingIds: number[],
		what: string,
		by: Stamp,
		edit: (r: Review, f: Finding) => Stamp | void,
	): Finding[] {
		return this.sql.tx(() => {
			const r = this.get(reviewId, false);
			const out: Finding[] = [];
			let lastAt = by.at;
			for (const fid of findingIds) {
				const f = this.getFinding(reviewId, fid);
				const stamp = edit(r, f) ?? by;
				lastAt = stamp.at;
				this.saveFinding(f, what, stamp, 0);
				out.push(f);
			}
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				lastAt,
				reviewId,
			);
			return out;
		});
	}

	addResponse(
		e: ReviewEvent,
		afterSeq: bigint,
		check: (r: Review, f: Finding | null) => void,
	): ReviewEvent {
		return this.sql.tx(() => {
			const r = this.get(e.reviewId, false);
			const f = e.findingId !== 0 ? this.getFinding(r.id, e.findingId) : null;
			check(r, f);
			if (afterSeq > 0n) {
				const newer = this.sql
					.exec(
						"SELECT seq FROM review_events WHERE review_id = ? AND seq > ? ORDER BY seq",
						r.id,
						afterSeq,
					)
					.toArray();
				if (newer.length > 0) {
					const latest = big(newer[newer.length - 1].seq);
					throw hotbedError(
						ErrorCode.Conflict,
						`review ${r.id} has newer events (latest is ${latest}, not ${afterSeq})`,
					).withSuggestion(`hotbed review responses ${r.id}`);
				}
			}
			const about = f ? `finding ${f.id} (${f.title}) of review ${r.id}` : `review ${r.id}`;
			e.what = "responded";
			e.seq = this.insertEvent(e, "");
			this.sql.exec(
				"UPDATE reviews SET updated_at = MAX(updated_at, ?) WHERE id = ?",
				e.by?.at ?? 0n,
				r.id,
			);
			const requester = r.requested?.session ?? "";
			if (requester && requester !== (e.by?.session ?? "")) {
				this.tellRequester(
					requester,
					e.by?.at ?? 0n,
					ReviewUpdate_What.RESPONDED,
					r.id,
					reviewResponseOn(about, e),
				);
			}
			return e;
		});
	}

	eventsAfter(reviewId: number, afterSeq: bigint): ReviewEvent[] {
		return this.sql
			.exec(
				"SELECT seq, finding_id, what, at, session, host, quick, text, by_doc, reviewer_id FROM review_events WHERE review_id = ? AND seq > ? ORDER BY seq",
				reviewId,
				afterSeq,
			)
			.toArray()
			.map((row) => {
				const byDoc = str(row.by_doc);
				let by: Stamp;
				if (byDoc) {
					by = unmarshal(StampSchema, byDoc);
				} else {
					const session = str(row.session);
					by = create(StampSchema, {
						at: big(row.at),
						session,
						host: str(row.host),
						actorKind: session ? ActorKind.AGENT : ActorKind.USER,
					});
				}
				return create(ReviewEventSchema, {
					seq: big(row.seq),
					reviewId,
					findingId: num(row.finding_id),
					what: str(row.what),
					by,
					quick: enumFromName(QuickResponseSchema, str(row.quick)),
					text: str(row.text),
					reviewerId: num(row.reviewer_id),
				});
			});
	}

	private getFinding(reviewId: number, fid: number): Finding {
		const rows = this.sql
			.exec("SELECT id, doc FROM review_findings WHERE review_id = ? AND id = ?", reviewId, fid)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `review ${reviewId} has no finding ${fid}`);
		}
		const f = unmarshal(FindingSchema, str(rows[0].doc));
		f.id = num(rows[0].id);
		f.reviewId = reviewId;
		return f;
	}

	private attachCountsAndDetails(r: Review, withDetail: boolean): void {
		const fRows = this.sql
			.exec("SELECT id, doc FROM review_findings WHERE review_id = ? ORDER BY id", r.id)
			.toArray();
		const findings: Finding[] = [];
		let important = 0;
		let preExisting = 0;
		let nits = 0;
		let open = 0;
		const byReviewer = new Map<number, number>();
		for (const fr of fRows) {
			const f = unmarshal(FindingSchema, str(fr.doc));
			f.id = num(fr.id);
			f.reviewId = r.id;
			if (f.severity === FindingSeverity.IMPORTANT) important++;
			else if (f.severity === FindingSeverity.PRE_EXISTING) preExisting++;
			else if (f.severity === FindingSeverity.NIT) nits++;
			if (f.decision === FindingDecision.OPEN || f.decision === FindingDecision.UNSPECIFIED) open++;
			const rvid = f.reviewerId || (r.reviewers.length === 1 ? r.reviewers[0].id : 0);
			if (rvid > 0) {
				byReviewer.set(rvid, (byReviewer.get(rvid) ?? 0) + 1);
			}
			findings.push(f);
		}
		for (const rv of r.reviewers) {
			rv.findingsCount = byReviewer.get(rv.id) ?? 0;
		}
		const events = this.eventsAfter(r.id, 0n);
		let responses = 0;
		const byFinding = new Map<number, ReviewEvent[]>();
		for (const ev of events) {
			if (ev.what === "responded") {
				responses++;
				if (ev.findingId !== 0) {
					let arr = byFinding.get(ev.findingId);
					if (!arr) {
						arr = [];
						byFinding.set(ev.findingId, arr);
					}
					arr.push(ev);
				}
			}
		}
		r.important = important;
		r.preExisting = preExisting;
		r.nits = nits;
		r.open = open;
		r.responses = responses;
		if (withDetail) {
			for (const f of findings) {
				const fResps = byFinding.get(f.id) ?? [];
				f.responses = fResps.length;
				for (const ev of fResps) {
					if (ev.quick !== QuickResponse.UNSPECIFIED) {
						f.quick = ev.quick;
						f.quickBy = ev.by;
					}
				}
			}
			r.findings = findings;
			r.events = events;
		}
	}

	private save(r: Review, what: string, by: Stamp, reviewerId = 0): void {
		const stored = clone(ReviewSchema, r);
		stored.project = "";
		stored.findings = [];
		stored.events = [];
		stored.important = 0;
		stored.preExisting = 0;
		stored.nits = 0;
		stored.open = 0;
		stored.responses = 0;
		for (const rv of stored.reviewers) rv.findingsCount = 0;
		const doc = marshal(ReviewSchema, stored);
		this.sql.exec(
			"UPDATE reviews SET state = ?, updated_at = ?, doc = ? WHERE id = ?",
			stateName(r.state),
			r.updatedAt,
			doc,
			r.id,
		);
		this.insertEvent(
			create(ReviewEventSchema, { reviewId: r.id, what, by, text: by.reason, reviewerId }),
			doc,
		);
	}

	private saveFinding(f: Finding, what: string, by: Stamp, reviewerId = 0): void {
		const stored = clone(FindingSchema, f);
		stored.responses = 0;
		stored.quickBy = undefined;
		const doc = marshal(FindingSchema, stored);
		this.sql.exec("UPDATE review_findings SET doc = ? WHERE id = ?", doc, f.id);
		this.insertEvent(
			create(ReviewEventSchema, {
				reviewId: f.reviewId,
				findingId: f.id,
				what,
				by,
				text: by.reason,
				reviewerId,
			}),
			doc,
		);
	}

	private insertEvent(e: ReviewEvent, doc: string): bigint {
		const byDoc = e.by ? marshal(StampSchema, e.by) : "";
		const seq = this.sql.insert(
			"INSERT INTO review_events (review_id, finding_id, what, at, session, host, quick, text, doc, by_doc, reviewer_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			e.reviewId,
			e.findingId,
			e.what,
			e.by?.at ?? 0n,
			e.by?.session ?? "",
			e.by?.host ?? "",
			enumName(QuickResponseSchema, e.quick),
			e.text,
			doc,
			byDoc,
			e.reviewerId,
		);
		return BigInt(seq);
	}
}
