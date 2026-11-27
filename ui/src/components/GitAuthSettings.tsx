import { Copy, ExternalLink, GitBranch, KeyRound } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { GitHubLoginChallenge } from "../api";
import { ago } from "../lib/format";
import {
  useCompleteGitHubLogin,
  useDeleteGitCredential,
  useGitCredentials,
  usePutGitCredential,
  useStartGitHubLogin,
} from "../lib/settings";
import { CopyButton } from "./CopyButton";
import { Field, TextInput } from "./ConfigSection";
import { Card, Empty } from "./Panel";

/** Repo patterns as typed: comma- or newline-separated, blanks dropped. */
function repoPatterns(raw: string): string[] {
  return raw
    .split(/[,\n]/)
    .map((r) => r.trim())
    .filter(Boolean);
}

function ErrorAlert({ error }: { error: unknown }) {
  if (!error) return null;
  return (
    <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
      <AlertDescription className="text-[11px] text-critical">{String(error)}</AlertDescription>
    </Alert>
  );
}

/**
 * The organization's git credentials: listed for every member, added (GitHub
 * device flow or a static token) and removed by admins. Credentials belong to
 * the organization as a whole, never to one node.
 */
export function GitAuthSettings({ orgLabel, canEdit, now }: { orgLabel: string; canEdit: boolean; now: number }) {
  const creds = useGitCredentials();
  const deleteCred = useDeleteGitCredential();
  return (
    <div className="space-y-5">
      <Card
        title="Organization Git Credentials"
        sub={`Stored write-only inside ${orgLabel}'s isolated Cloudflare Durable Object and delegated as short-lived tokens to this organization's workers, bases, and commands.`}
        right={
          <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
            <GitBranch className="mr-1 size-3" /> org-scoped
          </Badge>
        }
      >
        {creds.isLoading ? (
          <div className="py-6 text-center font-mono text-[11.5px] text-muted-foreground">loading git credentials…</div>
        ) : creds.error ? (
          <ErrorAlert error={creds.error} />
        ) : creds.data && creds.data.length > 0 ? (
          <div className="space-y-2">
            {creds.data.map((c) => (
              <div
                key={c.name}
                className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border/80 bg-background/40 px-3.5 py-2.5"
              >
                <div className="min-w-0 space-y-0.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-mono text-[12.5px] font-medium text-foreground">{c.name}</span>
                    <Badge variant="secondary" className="font-mono text-[10px]">
                      {c.host}
                    </Badge>
                    {c.refreshable && (
                      <Badge variant="outline" className="border-primary/40 font-mono text-[10px] text-primary">
                        OAuth refreshable
                      </Badge>
                    )}
                  </div>
                  <div className="font-mono text-[11px] text-muted-foreground">
                    {c.username ? `user=${c.username} · ` : ""}
                    repos={c.repos.length > 0 ? c.repos.join(", ") : "*"}
                    {c.updatedAt > 0n ? ` · updated ${ago(c.updatedAt, now)}` : ""}
                  </div>
                </div>
                {canEdit && (
                  <Button
                    type="button"
                    variant="outline"
                    size="xs"
                    disabled={deleteCred.isPending}
                    onClick={() => deleteCred.mutate(c.name)}
                    className="font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                  >
                    Remove
                  </Button>
                )}
              </div>
            ))}
          </div>
        ) : (
          <Empty>No git credentials configured for this organization yet.</Empty>
        )}
        <ErrorAlert error={deleteCred.error} />
      </Card>

      {canEdit && (
        <div className="grid gap-5 lg:grid-cols-2">
          <GitHubLoginCard />
          <StaticTokenCard />
        </div>
      )}
    </div>
  );
}

function GitHubLoginCard() {
  const start = useStartGitHubLogin();
  const complete = useCompleteGitHubLogin();
  const [form, setForm] = useState({ name: "github", host: "github.com", repos: "" });
  const [challenge, setChallenge] = useState<GitHubLoginChallenge | null>(null);

  async function onStart(e: FormEvent) {
    e.preventDefault();
    setChallenge(null);
    try {
      setChallenge(
        await start.mutateAsync({
          name: form.name.trim() || "github",
          host: form.host.trim() || "github.com",
          repos: repoPatterns(form.repos),
        }),
      );
    } catch {
      // Shown from start.error.
    }
  }

  async function onComplete() {
    if (!challenge) return;
    try {
      await complete.mutateAsync(challenge.loginId);
      setChallenge(null);
    } catch {
      // Shown from complete.error.
    }
  }

  return (
    <Card
      title="Sign in with GitHub (OAuth Device Flow)"
      sub="Recommended: mints a refreshable GitHub OAuth credential directly on this organization's controller."
    >
      <form onSubmit={onStart} className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-2">
          <Field id="gh-name" label="Credential name">
            <TextInput id="gh-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="github" required />
          </Field>
          <Field id="gh-host" label="Git host">
            <TextInput id="gh-host" value={form.host} onChange={(e) => setForm({ ...form, host: e.target.value })} placeholder="github.com" />
          </Field>
        </div>
        <Field id="gh-repos" label="Repo patterns (optional)" hint="Comma-separated owner/* or owner/repo; leave blank for all repos on host.">
          <TextInput
            id="gh-repos"
            value={form.repos}
            onChange={(e) => setForm({ ...form, repos: e.target.value })}
            placeholder="my-org/*, my-org/my-repo"
          />
        </Field>
        <Button type="submit" size="sm" disabled={start.isPending} className="gap-1.5 font-mono text-[11.5px]">
          <KeyRound className="size-3.5" />
          {start.isPending ? "Starting…" : "Start GitHub login"}
        </Button>
      </form>

      <ErrorAlert error={start.error ?? complete.error} />

      {challenge && (
        <Alert className="mt-4 rounded-xl border-primary/50 bg-ember-wash/60 p-3.5">
          <AlertTitle className="font-mono text-[11.5px] font-medium text-primary">Approve on GitHub, then click Complete</AlertTitle>
          <AlertDescription className="mt-2 space-y-2.5 text-[12px]">
            <div className="flex flex-wrap items-center gap-2">
              <code className="rounded-lg border border-primary/40 bg-background px-3 py-1 font-mono text-[15px] font-semibold tracking-wider text-foreground">
                {challenge.userCode}
              </code>
              <CopyButton text={challenge.userCode} label="Copy code" icon={<Copy className="size-3" />} />
              <a
                href={challenge.verificationUri}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 font-mono text-[11px] text-primary underline underline-offset-4"
              >
                {challenge.verificationUri}
                <ExternalLink className="size-3" />
              </a>
            </div>
            <div className="flex items-center gap-2">
              <Button type="button" size="sm" disabled={complete.isPending} onClick={() => void onComplete()} className="font-mono text-[11.5px]">
                {complete.isPending ? "Waiting for GitHub…" : "Complete login"}
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => setChallenge(null)}
                className="font-mono text-[11px] text-muted-foreground"
              >
                Cancel
              </Button>
            </div>
          </AlertDescription>
        </Alert>
      )}
    </Card>
  );
}

const EMPTY_TOKEN_FORM = { name: "", host: "github.com", user: "", repos: "", api: "", token: "" };

function StaticTokenCard() {
  const put = usePutGitCredential();
  const [form, setForm] = useState(EMPTY_TOKEN_FORM);
  const ready = form.name.trim() !== "" && form.token.trim() !== "";

  async function onSave(e: FormEvent) {
    e.preventDefault();
    if (!ready) return;
    try {
      await put.mutateAsync({
        name: form.name.trim(),
        host: form.host.trim() || "github.com",
        username: form.user.trim(),
        repos: repoPatterns(form.repos),
        apiUrl: form.api.trim(),
        token: form.token.trim(),
      });
      // Keep the host and username for the next token; clear the rest.
      setForm((f) => ({ ...EMPTY_TOKEN_FORM, host: f.host, user: f.user }));
    } catch {
      // Shown from put.error.
    }
  }

  const input = (key: keyof typeof EMPTY_TOKEN_FORM, placeholder: string, extra?: { type?: string; required?: boolean }) => (
    <TextInput
      id={`pat-${key}`}
      value={form[key]}
      onChange={(e) => setForm({ ...form, [key]: e.target.value })}
      placeholder={placeholder}
      {...extra}
    />
  );

  return (
    <Card
      title="Add Static Git Token"
      sub="Store a personal access token or fine-grained token write-only for headless CI or non-GitHub hosts."
    >
      <form onSubmit={onSave} className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-2">
          <Field id="pat-name" label="Credential name">
            {input("name", "github-ci", { required: true })}
          </Field>
          <Field id="pat-host" label="Git host">
            {input("host", "github.com")}
          </Field>
          <Field id="pat-user" label="Username (optional)">
            {input("user", "x-access-token")}
          </Field>
          <Field id="pat-repos" label="Repo patterns (optional)">
            {input("repos", "owner/* (blank = all)")}
          </Field>
        </div>
        <Field id="pat-api" label="API base URL (optional, for GHE)">
          {input("api", "https://github.company.com/api/v3")}
        </Field>
        <Field id="pat-token" label="Token (stored write-only)">
          {input("token", "ghp_… or github_pat_…", { type: "password", required: true })}
        </Field>
        <Button type="submit" size="sm" disabled={put.isPending || !ready} className="font-mono text-[11.5px]">
          {put.isPending ? "Saving…" : "Save credential"}
        </Button>
      </form>
      <ErrorAlert error={put.error} />
    </Card>
  );
}
