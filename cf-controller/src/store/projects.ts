import {
	ProjectChangeSchema,
	ProjectSchema,
	RepoSchema,
	StampSchema,
	type Project,
	type ProjectChange,
	type Repo,
	type Stamp,
} from "../gen/hotbed/v1/hotbed_pb.ts";
import { big, num, str } from "../sql-values.ts";
import { ErrorCode, hotbedError } from "../errors.ts";
import { create, clone } from "@bufbuild/protobuf";
import { marshal, unmarshal, type Row, type SqlKernel } from "./kernel.ts";

/** Projects (their repos, setup and change log) and the cluster's registered repos. */
export class ProjectStore {
	constructor(private readonly sql: SqlKernel) {}

	putRepo(repo: Repo): void {
		const copy = clone(RepoSchema, repo);
		copy.reminder = undefined;
		this.sql.exec(
			"INSERT INTO repos (id, doc) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET doc = excluded.doc",
			repo.id,
			marshal(RepoSchema, copy),
		);
	}

	deleteRepo(id: string): boolean {
		const cur = this.sql.exec("DELETE FROM repos WHERE id = ?", id);
		return cur.rowsWritten > 0;
	}

	listRepos(): Repo[] {
		return this.sql
			.exec("SELECT doc FROM repos ORDER BY id")
			.toArray()
			.map((r) => unmarshal(RepoSchema, str(r.doc)));
	}

	// ---- projects -------------------------------------------------------

	private recordChange(pid: number, what: string, repo: string, by: Stamp): void {
		this.sql.exec(
			"INSERT INTO project_changes (project_id, what, repo, at, session, host, reason) VALUES (?, ?, ?, ?, ?, ?, ?)",
			pid,
			what,
			repo,
			by.at,
			by.session,
			by.host,
			by.reason,
		);
	}

	idByName(name: string): number {
		const rows = this.sql.exec("SELECT id FROM projects WHERE name = ?", name).toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no project ${name}`).withSuggestion(
				"hotbed project ls",
			);
		}
		return num(rows[0].id);
	}

	add(name: string, remotes: string[], by: Stamp): Project {
		return this.sql.tx(() => {
			const ins = this.sql.exec(
				"INSERT INTO projects (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING",
				name,
				by.at,
			);
			const pid = this.idByName(name);
			if (ins.rowsWritten > 0) {
				this.recordChange(pid, "created", "", by);
			}
			for (const remote of remotes) {
				const added = this.sql.exec(
					"INSERT INTO project_repos (project_id, remote) VALUES (?, ?) ON CONFLICT DO NOTHING",
					pid,
					remote,
				);
				if (added.rowsWritten > 0) {
					this.recordChange(pid, "added", remote, by);
				}
			}
			return this.get(name);
		});
	}

	removeRepos(name: string, remotes: string[], by: Stamp): Project {
		return this.sql.tx(() => {
			const pid = this.idByName(name);
			for (const remote of remotes) {
				const del = this.sql.exec(
					"DELETE FROM project_repos WHERE project_id = ? AND remote = ?",
					pid,
					remote,
				);
				if (del.rowsWritten === 0) {
					throw hotbedError(
						ErrorCode.NotFound,
						`project ${name} does not hold ${remote}`,
					).withSuggestion(`hotbed project show ${name}`);
				}
				this.recordChange(pid, "removed", remote, by);
			}
			return this.get(name);
		});
	}

	editSetup(name: string, edit: (p: Project) => void): Project {
		return this.sql.tx(() => {
			const p = this.get(name);
			edit(p);
			p.setupRevision += 1n;
			const setupOnly = create(ProjectSchema, {
				steps: p.steps,
				hosts: p.hosts,
				scratchEpoch: p.scratchEpoch,
				setupRevision: p.setupRevision,
			});
			const pid = this.idByName(name);
			this.sql.exec(
				"UPDATE projects SET setup = ? WHERE id = ?",
				marshal(ProjectSchema, setupOnly),
				pid,
			);
			return p;
		});
	}

	get(name: string): Project {
		const rows = this.sql
			.exec("SELECT id, name, created_at, setup FROM projects WHERE name = ?", name)
			.toArray();
		if (rows.length === 0) {
			throw hotbedError(ErrorCode.NotFound, `no project ${name}`).withSuggestion(
				"hotbed project ls",
			);
		}
		return this.hydrate(rows[0]);
	}

	list(): Project[] {
		const rows = this.sql
			.exec("SELECT id, name, created_at, setup FROM projects ORDER BY name")
			.toArray();
		const repoRows = this.sql
			.exec("SELECT project_id, remote FROM project_repos ORDER BY project_id, remote")
			.toArray();
		const byPid = new Map<number, string[]>();
		for (const r of repoRows) {
			const pid = num(r.project_id);
			let list = byPid.get(pid);
			if (!list) {
				list = [];
				byPid.set(pid, list);
			}
			list.push(str(r.remote));
		}
		return rows.map((r) => this.fromRow(r, byPid.get(num(r.id)) ?? []));
	}

	holding(remote: string): Project[] {
		const rows = this.sql
			.exec(
				`SELECT p.id, p.name, p.created_at, p.setup
				 FROM projects p
				 JOIN project_repos r ON r.project_id = p.id
				 WHERE r.remote = ?
				 ORDER BY p.name`,
				remote,
			)
			.toArray();
		return rows.map((r) => this.hydrate(r));
	}

	history(name: string): ProjectChange[] {
		const pid = this.idByName(name);
		return this.sql
			.exec(
				"SELECT seq, what, repo, at, session, host, reason FROM project_changes WHERE project_id = ? ORDER BY seq",
				pid,
			)
			.toArray()
			.map((r) =>
				create(ProjectChangeSchema, {
					seq: big(r.seq),
					what: str(r.what),
					repo: str(r.repo),
					by: create(StampSchema, {
						at: big(r.at),
						session: str(r.session),
						host: str(r.host),
						reason: str(r.reason),
					}),
				}),
			);
	}

	private hydrate(r: Row): Project {
		const pid = num(r.id);
		const remotes = this.sql
			.exec("SELECT remote FROM project_repos WHERE project_id = ? ORDER BY remote", pid)
			.toArray()
			.map((rr) => str(rr.remote));
		return this.fromRow(r, remotes);
	}

	private fromRow(r: Row, remotes: string[]): Project {
		const setup = unmarshal(ProjectSchema, str(r.setup) || "{}");
		return create(ProjectSchema, {
			name: str(r.name),
			repos: remotes,
			createdAt: big(r.created_at),
			steps: setup.steps,
			hosts: setup.hosts,
			scratchEpoch: setup.scratchEpoch,
			setupRevision: setup.setupRevision,
		});
	}
}
