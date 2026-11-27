import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Card, Empty } from "../components/Panel";
import {
  useApiTokens,
  useAuthMe,
  useCreateApiToken,
  useCreateJoinToken,
  useCreateOrganization,
  useInviteMember,
  useJoinTokens,
  useOrgMembers,
  useRemoveMember,
  useRevokeApiToken,
  useRevokeInvitation,
  useRevokeJoinToken,
  useSwitchOrganization,
  type MintedApiToken,
  type MintedJoinToken,
} from "../lib/auth";
import { useSnapshot } from "../lib/data";
import { useProject } from "../lib/project";
import { PageTitle } from "./Nodes";

function formatDate(ms: number | string | null | undefined): string {
  if (!ms) return "—";
  const d = typeof ms === "number" ? new Date(ms) : new Date(ms);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}

const INVITE_ROLES = [
  { value: "viewer", label: "viewer" },
  { value: "member", label: "member" },
  { value: "admin", label: "admin" },
];

export function OrganizationPage() {
  const { data: auth } = useAuthMe();
  const { data: snap } = useSnapshot();
  const [, chooseProject] = useProject();
  const workosEnabled = Boolean(auth?.workosEnabled);
  const activeOrg = auth?.organization ?? null;
  const isOrgAdmin = !workosEnabled || activeOrg?.role === "admin" || activeOrg?.role === "owner";
  const isViewer = workosEnabled && activeOrg?.role === "viewer";

  const { data: team, isLoading: loadingTeam, error: teamErr } = useOrgMembers(Boolean(workosEnabled && activeOrg));
  const { data: joinTokens, isLoading: loadingJoinTokens, error: joinTokensErr } = useJoinTokens();
  const { data: apiTokens, isLoading: loadingApiTokens, error: apiTokensErr } = useApiTokens();

  const switchOrg = useSwitchOrganization();
  const createOrg = useCreateOrganization();
  const inviteMember = useInviteMember();
  const revokeInv = useRevokeInvitation();
  const removeMember = useRemoveMember();
  const createJoinToken = useCreateJoinToken();
  const revokeJoinToken = useRevokeJoinToken();
  const createApiToken = useCreateApiToken();
  const revokeApiToken = useRevokeApiToken();

  const [newOrgName, setNewOrgName] = useState("");
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("member");
  const [joinLabel, setJoinLabel] = useState("");
  const [joinHours, setJoinHours] = useState("24");
  const [mintedJoin, setMintedJoin] = useState<MintedJoinToken | null>(null);
  const [apiName, setApiName] = useState("");
  const [apiDays, setApiDays] = useState("90");
  const [mintedApi, setMintedApi] = useState<MintedApiToken | null>(null);
  const [copied, setCopied] = useState<string | null>(null);

  const origin = typeof window !== "undefined" ? window.location.origin : "https://hotbed-controller";

  async function copy(key: string, value: string) {
    let ok = false;
    if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
      try {
        await navigator.clipboard.writeText(value);
        ok = true;
      } catch {
        ok = false;
      }
    }
    if (!ok && typeof document !== "undefined") {
      try {
        const ta = document.createElement("textarea");
        ta.value = value;
        ta.setAttribute("readonly", "");
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        ok = document.execCommand("copy");
        document.body.removeChild(ta);
      } catch {
        ok = false;
      }
    }
    if (ok) {
      setCopied(key);
      setTimeout(() => setCopied((c) => (c === key ? null : c)), 1800);
    }
  }

  async function onCreateOrg(e: FormEvent) {
    e.preventDefault();
    const name = newOrgName.trim();
    if (!name) return;
    try {
      await createOrg.mutateAsync(name);
      chooseProject(null);
      setNewOrgName("");
    } catch {
      // Error is exposed on createOrg.error and rendered in the card.
    }
  }

  async function onInvite(e: FormEvent) {
    e.preventDefault();
    const email = inviteEmail.trim();
    if (!email) return;
    try {
      await inviteMember.mutateAsync({ email, role: inviteRole });
      setInviteEmail("");
    } catch {
      // Error is exposed on inviteMember.error and rendered in the card.
    }
  }

  async function onMintJoin(e: FormEvent) {
    e.preventDefault();
    const hrs = Number(joinHours);
    try {
      const tok = await createJoinToken.mutateAsync({
        label: joinLabel.trim() || undefined,
        expiresInHours: Number.isFinite(hrs) && hrs > 0 ? hrs : undefined,
      });
      setMintedJoin(tok);
      setJoinLabel("");
    } catch {
      // Error is exposed on createJoinToken.error and rendered in the card.
    }
  }

  async function onMintApi(e: FormEvent) {
    e.preventDefault();
    const name = apiName.trim();
    if (!name) return;
    const days = Number(apiDays);
    try {
      const tok = await createApiToken.mutateAsync({
        name,
        expiresInDays: Number.isFinite(days) && days > 0 ? days : undefined,
      });
      setMintedApi(tok);
      setApiName("");
    } catch {
      // Error is exposed on createApiToken.error and rendered in the card.
    }
  }

  return (
    <div className="space-y-6">
      <PageTitle
        title={activeOrg ? activeOrg.name : "Organization & Access"}
        sub={
          activeOrg
            ? `Isolated Cloudflare Durable Object · ${activeOrg.id} · cluster ${snap?.clusterId ?? "…"} · signed in as ${auth?.user.email ?? ""}`
            : `Cluster ${snap?.clusterId ?? "…"} · manage worker join tokens and API tokens`
        }
        right={
          <Link
            to="/settings"
            className="inline-flex h-8 items-center rounded-lg border border-border bg-card/70 px-3 font-mono text-[11.5px] text-foreground transition-colors hover:border-primary/50 hover:text-primary"
          >
            Org settings →
          </Link>
        }
      />

      {workosEnabled && (
        <div className="grid gap-6 lg:grid-cols-2">
          <Card
            title="Organizations"
            sub="Each organization runs in its own isolated Cloudflare Durable Object."
          >
            {auth?.organizations && auth.organizations.length > 0 ? (
              <div className="space-y-2">
                {auth.organizations.map((org) => {
                  const isCurrent = org.id === activeOrg?.id;
                  return (
                    <div
                      key={org.id}
                      className={`flex items-center justify-between gap-3 rounded-xl border px-3.5 py-2.5 ${
                        isCurrent ? "border-primary/60 bg-primary/5" : "border-border bg-background/40"
                      }`}
                    >
                      <div className="min-w-0">
                        <div className="truncate text-[13px] font-medium text-foreground">{org.name}</div>
                        <div className="font-mono text-[11px] text-muted-foreground">
                          {org.role} · {org.id}
                        </div>
                      </div>
                      {isCurrent ? (
                        <Badge variant="secondary" className="bg-primary/15 font-mono text-[10.5px] text-primary">
                          active
                        </Badge>
                      ) : (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={switchOrg.isPending}
                          onClick={() => {
                            chooseProject(null);
                            switchOrg.mutate(org.id);
                          }}
                          className="font-mono text-[11px]"
                        >
                          Switch
                        </Button>
                      )}
                    </div>
                  );
                })}
              </div>
            ) : (
              <Empty>No organizations yet.</Empty>
            )}

            <form onSubmit={onCreateOrg} className="mt-4 flex flex-col gap-2 border-t border-border pt-4 sm:flex-row">
              <Input
                type="text"
                value={newOrgName}
                onChange={(e) => setNewOrgName(e.target.value)}
                placeholder="New organization name…"
                className="min-w-0 flex-1 bg-background text-[12.5px]"
                required
              />
              <Button
                type="submit"
                disabled={createOrg.isPending || !newOrgName.trim()}
                className="w-full text-[12px] sm:w-auto"
              >
                {createOrg.isPending ? "Creating…" : "Create org"}
              </Button>
            </form>
            {(switchOrg.error || createOrg.error) && (
              <Alert variant="destructive" className="mt-2 border-critical/40 font-mono text-[11px]">
                <AlertDescription className="text-[11px] text-critical">
                  {String(switchOrg.error ?? createOrg.error)}
                </AlertDescription>
              </Alert>
            )}
          </Card>

          <Card
            title="Members & Invitations"
            sub="Multiple users in this organization share its cluster, nodes, projects, and sessions."
          >
            {isOrgAdmin && (
              <form onSubmit={onInvite} className="mb-4 flex flex-wrap gap-2">
                <Input
                  type="email"
                  value={inviteEmail}
                  onChange={(e) => setInviteEmail(e.target.value)}
                  placeholder="teammate@company.com"
                  className="min-w-[180px] flex-1 bg-background text-[12.5px]"
                  required
                />
                <Select items={INVITE_ROLES} value={inviteRole} onValueChange={(v) => v && setInviteRole(v)}>
                  <SelectTrigger aria-label="Role" className="w-28 bg-background font-mono text-[11.5px]">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent className="font-mono text-[11.5px]">
                    {INVITE_ROLES.map((r) => (
                      <SelectItem key={r.value} value={r.value} className="text-[11.5px]">
                        {r.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <Button
                  type="submit"
                  disabled={inviteMember.isPending || !inviteEmail.trim()}
                  className="text-[12px]"
                >
                  {inviteMember.isPending ? "Sending…" : "Invite"}
                </Button>
              </form>
            )}
            {(inviteMember.error || removeMember.error || revokeInv.error) && (
              <Alert variant="destructive" className="mb-3 border-critical/40 font-mono text-[11px]">
                <AlertDescription className="text-[11px] text-critical">
                  {String(inviteMember.error ?? removeMember.error ?? revokeInv.error)}
                </AlertDescription>
              </Alert>
            )}

            {loadingTeam ? (
              <div className="py-6 text-center font-mono text-[11.5px] text-muted-foreground">loading members…</div>
            ) : teamErr ? (
              <Alert variant="destructive" className="border-critical/40 font-mono text-[11.5px]">
                <AlertDescription className="text-[11.5px] text-critical">{String(teamErr)}</AlertDescription>
              </Alert>
            ) : (
              <div className="space-y-4">
                <div className="space-y-1.5">
                  {(team?.members ?? []).map((m) => (
                    <div
                      key={m.membershipId}
                      className="flex items-center justify-between gap-2 rounded-lg border border-border/70 bg-background/40 px-3 py-2"
                    >
                      <div className="min-w-0">
                        <div className="truncate text-[12.5px] font-medium text-foreground">
                          {m.name}
                          {m.userId === auth?.user.id && (
                            <span className="ml-1.5 font-mono text-[10.5px] text-muted-foreground">(you)</span>
                          )}
                        </div>
                        <div className="truncate font-mono text-[11px] text-muted-foreground">
                          {m.email} · {m.role}
                        </div>
                      </div>
                      {isOrgAdmin && m.userId !== auth?.user.id && (
                        <Button
                          type="button"
                          variant="outline"
                          size="xs"
                          onClick={() => removeMember.mutate(m.membershipId)}
                          className="font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                        >
                          Remove
                        </Button>
                      )}
                    </div>
                  ))}
                </div>

                {team && team.invitations.length > 0 && (
                  <div className="space-y-1.5 border-t border-border pt-3">
                    <div className="eyebrow">Pending invitations</div>
                    {team.invitations.map((inv) => (
                      <div
                        key={inv.id}
                        className="flex items-center justify-between gap-2 rounded-lg border border-dashed border-border px-3 py-2"
                      >
                        <div className="min-w-0">
                          <div className="truncate text-[12px] text-foreground">{inv.email}</div>
                          <div className="font-mono text-[10.5px] text-muted-foreground">
                            {inv.role} · expires {formatDate(inv.expiresAt)}
                          </div>
                        </div>
                        {isOrgAdmin && (
                          <Button
                            type="button"
                            variant="outline"
                            size="xs"
                            onClick={() => revokeInv.mutate(inv.id)}
                            className="font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                          >
                            Revoke
                          </Button>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
          </Card>
        </div>
      )}

      <div className="grid gap-6 lg:grid-cols-2">
        <Card
          title="Worker Join Tokens"
          sub="Org-scoped tokens (hbj_...) used to join Linux or macOS workers to this cluster. Exchanged for a node-bound worker token on join."
        >
          {!isViewer && (
            <form onSubmit={onMintJoin} className="mb-4 flex flex-wrap gap-2">
              <Input
                type="text"
                value={joinLabel}
                onChange={(e) => setJoinLabel(e.target.value)}
                placeholder="Label (e.g. build-box)"
                className="min-w-[140px] flex-1 bg-background text-[12.5px]"
              />
              <Input
                type="number"
                min="1"
                value={joinHours}
                onChange={(e) => setJoinHours(e.target.value)}
                title="Expiry in hours"
                className="w-20 bg-background font-mono text-[12px]"
              />
              <span className="self-center font-mono text-[11px] text-muted-foreground">hrs</span>
              <Button
                type="submit"
                disabled={createJoinToken.isPending}
                className="text-[12px]"
              >
                {createJoinToken.isPending ? "Minting…" : "Mint join token"}
              </Button>
            </form>
          )}

          {(createJoinToken.error || revokeJoinToken.error) && (
            <Alert variant="destructive" className="mb-3 border-critical/40 font-mono text-[11px]">
              <AlertDescription className="text-[11px] text-critical">
                {String(createJoinToken.error ?? revokeJoinToken.error)}
              </AlertDescription>
            </Alert>
          )}

          {mintedJoin && (
            <Alert className="mb-4 rounded-xl border-primary/50 bg-primary/5 p-3">
              <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
                <AlertTitle className="font-mono text-[11px] font-medium text-primary">
                  Copy now — shown once
                </AlertTitle>
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  onClick={() =>
                    void copy(
                      "join",
                      `sudo hotbed join ${origin} /var/lib/hotbed --user $USER --token ${mintedJoin.token}`,
                    )
                  }
                  className="shrink-0 border-primary/40 font-mono text-[10.5px] text-primary hover:bg-primary/10"
                >
                  {copied === "join" ? "Copied" : "Copy join command"}
                </Button>
              </div>
              <AlertDescription className="mt-1.5 min-w-0">
                <pre className="max-w-full overflow-x-auto font-mono text-[11px] text-foreground">
                  {`sudo hotbed join ${origin} /var/lib/hotbed --user $USER --token ${mintedJoin.token}`}
                </pre>
              </AlertDescription>
            </Alert>
          )}

          {loadingJoinTokens ? (
            <div className="py-6 text-center font-mono text-[11.5px] text-muted-foreground">loading join tokens…</div>
          ) : joinTokensErr ? (
            <Alert variant="destructive" className="border-critical/40 font-mono text-[11px]">
              <AlertDescription className="text-[11px] text-critical">{String(joinTokensErr)}</AlertDescription>
            </Alert>
          ) : joinTokens && joinTokens.tokens.length > 0 ? (
            <div className="space-y-1.5">
              {joinTokens.tokens.map((t) => (
                <div
                  key={t.id}
                  className="flex items-center justify-between gap-2 rounded-lg border border-border/70 bg-background/40 px-3 py-2"
                >
                  <div className="min-w-0">
                    <div className="truncate text-[12px] font-medium text-foreground">
                      {t.label || t.id}
                    </div>
                    <div className="font-mono text-[10.5px] text-muted-foreground">
                      {t.createdBy || "owner"} · created {formatDate(t.createdAt)}
                      {t.expiresAt ? ` · exp ${formatDate(t.expiresAt)}` : ""}
                      {t.revokedAt ? " · revoked" : ""}
                    </div>
                  </div>
                  {!isViewer && !t.revokedAt && (
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      disabled={revokeJoinToken.isPending}
                      onClick={() => revokeJoinToken.mutate(t.id)}
                      className="font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                    >
                      Revoke
                    </Button>
                  )}
                </div>
              ))}
            </div>
          ) : (
            <Empty>No worker join tokens minted yet.</Empty>
          )}
        </Card>

        <Card
          title="Personal API Tokens"
          sub="Org-scoped bearer tokens (hbu_...) for CI, scripts, and headless agents via HOTBED_TOKEN."
        >
          <form onSubmit={onMintApi} className="mb-4 flex flex-wrap gap-2">
            <Input
              type="text"
              value={apiName}
              onChange={(e) => setApiName(e.target.value)}
              placeholder="Token name (e.g. github-ci)"
              className="min-w-[140px] flex-1 bg-background text-[12.5px]"
              required
            />
            <Input
              type="number"
              min="1"
              value={apiDays}
              onChange={(e) => setApiDays(e.target.value)}
              title="Expiry in days"
              className="w-20 bg-background font-mono text-[12px]"
            />
            <span className="self-center font-mono text-[11px] text-muted-foreground">days</span>
            <Button
              type="submit"
              disabled={createApiToken.isPending || !apiName.trim()}
              className="text-[12px]"
            >
              {createApiToken.isPending ? "Creating…" : "Create token"}
            </Button>
          </form>

          {(createApiToken.error || revokeApiToken.error) && (
            <Alert variant="destructive" className="mb-3 border-critical/40 font-mono text-[11px]">
              <AlertDescription className="text-[11px] text-critical">
                {String(createApiToken.error ?? revokeApiToken.error)}
              </AlertDescription>
            </Alert>
          )}

          {mintedApi && (
            <Alert className="mb-4 rounded-xl border-primary/50 bg-primary/5 p-3">
              <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
                <AlertTitle className="font-mono text-[11px] font-medium text-primary">
                  Copy now — shown once
                </AlertTitle>
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  onClick={() => void copy("api", mintedApi.token)}
                  className="shrink-0 border-primary/40 font-mono text-[10.5px] text-primary hover:bg-primary/10"
                >
                  {copied === "api" ? "Copied" : "Copy token"}
                </Button>
              </div>
              <AlertDescription className="mt-1.5 min-w-0">
                <pre className="max-w-full overflow-x-auto font-mono text-[11px] text-foreground">{mintedApi.token}</pre>
              </AlertDescription>
            </Alert>
          )}

          {loadingApiTokens ? (
            <div className="py-6 text-center font-mono text-[11.5px] text-muted-foreground">loading API tokens…</div>
          ) : apiTokensErr ? (
            <Alert variant="destructive" className="border-critical/40 font-mono text-[11px]">
              <AlertDescription className="text-[11px] text-critical">{String(apiTokensErr)}</AlertDescription>
            </Alert>
          ) : apiTokens && apiTokens.tokens.length > 0 ? (
            <div className="space-y-1.5">
              {apiTokens.tokens.map((t) => (
                <div
                  key={t.id}
                  className="flex items-center justify-between gap-2 rounded-lg border border-border/70 bg-background/40 px-3 py-2"
                >
                  <div className="min-w-0">
                    <div className="truncate text-[12px] font-medium text-foreground">{t.name}</div>
                    <div className="font-mono text-[10.5px] text-muted-foreground">
                      {t.email} · created {formatDate(t.createdAt)}
                      {t.expiresAt ? ` · exp ${formatDate(t.expiresAt)}` : ""}
                      {t.revokedAt ? " · revoked" : ""}
                    </div>
                  </div>
                  {!t.revokedAt && (
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      disabled={revokeApiToken.isPending}
                      onClick={() => revokeApiToken.mutate(t.id)}
                      className="font-mono text-[10.5px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                    >
                      Revoke
                    </Button>
                  )}
                </div>
              ))}
            </div>
          ) : (
            <Empty>No personal API tokens created yet.</Empty>
          )}
        </Card>
      </div>
    </div>
  );
}
