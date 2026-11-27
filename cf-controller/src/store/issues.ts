import { IssueSchema, IssueState, type Issue } from "../gen/hotbed/v1/hotbed_pb.ts";
import { num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { marshal, unmarshal, type SqlKernel } from "./kernel.ts";

function issueStateName(st: IssueState): string {
	return st === IssueState.ARCHIVED ? "archived" : "open";
}

/** Issues agents report with hotbed itself. */
export class IssueStore {
	constructor(private readonly sql: SqlKernel) {}

	/** Files a new issue, numbering it. */
	add(issue: Issue): Issue {
		return this.sql.tx(() => {
			issue.id = this.sql.insert(
				"INSERT INTO issues (state, votes, updated_at, doc) VALUES (?, 0, ?, '')",
				issueStateName(issue.state),
				issue.updatedAt,
			);
			this.save(issue);
			return issue;
		});
	}

	save(issue: Issue): void {
		this.sql.exec(
			"UPDATE issues SET state = ?, votes = ?, updated_at = ?, doc = ? WHERE id = ?",
			issueStateName(issue.state),
			issue.votes,
			issue.updatedAt,
			marshal(IssueSchema, issue),
			issue.id,
		);
	}

	get(id: number): Issue {
		const rows = this.sql.exec("SELECT doc FROM issues WHERE id = ?", id).toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no issue ${id}`).withSuggestion(
				"list them: hotbed issue ls --all",
			);
		}
		return unmarshal(IssueSchema, str(rows[0].doc));
	}

	/** Issues in state (any, when undefined), most voted first, then most recently changed. */
	list(state?: IssueState): Issue[] {
		const rows =
			state === undefined
				? this.sql.exec("SELECT doc FROM issues ORDER BY votes DESC, updated_at DESC").toArray()
				: this.sql
						.exec(
							"SELECT doc FROM issues WHERE state = ? ORDER BY votes DESC, updated_at DESC",
							issueStateName(state),
						)
						.toArray();
		return rows.map((r) => unmarshal(IssueSchema, str(r.doc)));
	}

	openCount(): number {
		return num(
			this.sql
				.exec("SELECT COUNT(*) AS n FROM issues WHERE state = ?", issueStateName(IssueState.OPEN))
				.one().n,
		);
	}
}
