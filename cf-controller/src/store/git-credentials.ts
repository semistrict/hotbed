import { GitCredentialSchema, type GitCredential } from "../gen/hotbed/v1/hotbed_pb.ts";
import { str } from "../sql-values.ts";
import { marshal, unmarshal, type SqlKernel } from "./kernel.ts";

export interface GitCredentialSecret {
	token: string;
	refreshToken?: string;
	clientId?: string;
	tokenExpiresAt?: number;
	refreshExpiresAt?: number;
}

export interface StoredGitCredential {
	cred: GitCredential;
	secret: GitCredentialSecret;
}

/** Git credentials by name, with their secrets, served to workers by host. */
export class GitCredentialStore {
	constructor(private readonly sql: SqlKernel) {}

	put(cred: GitCredential, secret: GitCredentialSecret): void {
		this.sql.exec(
			`INSERT INTO git_credentials (name, host, doc, secret) VALUES (?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET host = excluded.host, doc = excluded.doc, secret = excluded.secret`,
			cred.name,
			cred.host,
			marshal(GitCredentialSchema, cred),
			JSON.stringify(secret),
		);
	}

	delete(name: string): boolean {
		return this.sql.exec("DELETE FROM git_credentials WHERE name = ?", name).rowsWritten > 0;
	}

	list(): GitCredential[] {
		return this.sql
			.exec("SELECT doc FROM git_credentials ORDER BY name")
			.toArray()
			.map((r) => unmarshal(GitCredentialSchema, str(r.doc)));
	}

	byHost(host: string): StoredGitCredential[] {
		return this.sql
			.exec("SELECT doc, secret FROM git_credentials WHERE host = ? ORDER BY name", host)
			.toArray()
			.map((r) => ({
				cred: unmarshal(GitCredentialSchema, str(r.doc)),
				secret: JSON.parse(str(r.secret)) as GitCredentialSecret,
			}));
	}
}
