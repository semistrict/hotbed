import { useNavigate, useSearch } from "@tanstack/react-router";
import {
  Check,
  Copy,
  ExternalLink,
  GitBranch,
  KeyRound,
  RefreshCw,
  RotateCcw,
  Save,
  Server,
  ShieldAlert,
  SlidersHorizontal,
  Trash2,
  Zap,
} from "lucide-react";
import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import type { GitHubLoginChallenge, NodeSystemUpgrade, WorkerConfig } from "../api";
import { Card, Empty } from "../components/Panel";
import { useAuthMe } from "../lib/auth";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import {
  ARCH_KEYS,
  BUILTIN_DEFAULTS,
  draftRequiresRestart,
  draftToCliCommand,
  draftToWorkerConfigInput,
  formatBytesHint,
  formatDurationSeconds,
  formatSecondsHint,
  parseWorkerConfigJson,
  useCompleteGitHubLogin,
  useDeleteGitCredential,
  useDeleteWorkerConfig,
  useGitCredentials,
  usePutGitCredential,
  useSetWorkerConfig,
  useStartGitHubLogin,
  useUpgradeSystem,
  useWorkerConfig,
  workerConfigToDraft,
  workerConfigToPrettyJson,
  type ArchKey,
  type ArtifactDraft,
  type ArtifactMapDraft,
  type SettingsDraft,
  type TriState,
} from "../lib/settings";
import { PageTitle } from "./Nodes";

export type SettingsTab = "freshen" | "runtime" | "vm" | "fuse" | "system" | "git" | "json";

export interface SettingsSearch {
  scope?: string;
  tab?: SettingsTab;
}

const VALID_TABS: readonly SettingsTab[] = ["freshen", "runtime", "vm", "fuse", "system", "git", "json"];

const RUNTIME_ITEMS = [
  { value: "__inherit__", label: "Inherit / default" },
  { value: "vm", label: "vm (Firecracker microVM)" },
  { value: "cladefs", label: "cladefs (host FUSE)" },
];

const CACHE_TYPE_ITEMS = [
  { value: "__inherit__", label: "Inherit (Unsafe)" },
  { value: "Unsafe", label: "Unsafe (non-blocking fsync)" },
  { value: "Writeback", label: "Writeback" },
];

function inheritedTriLabel(inherited: boolean | undefined, builtin: boolean): string {
  if (inherited === true) return "Inherit (enabled)";
  if (inherited === false) return "Inherit (disabled)";
  return builtin ? "Default (enabled)" : "Default (disabled)";
}

function TriSelect({
  id,
  value,
  onChange,
  disabled,
  inheritLabel,
  trueLabel = "Enabled",
  falseLabel = "Disabled",
}: {
  id: string;
  value: TriState;
  onChange: (next: TriState) => void;
  disabled?: boolean;
  inheritLabel: string;
  trueLabel?: string;
  falseLabel?: string;
}) {
  const items = [
    { value: "inherit", label: inheritLabel },
    { value: "true", label: trueLabel },
    { value: "false", label: falseLabel },
  ];
  return (
    <Select items={items} value={value} disabled={disabled} onValueChange={(v) => v && onChange(v as TriState)}>
      <SelectTrigger id={id} className="w-full bg-background font-mono text-[12px]">
        <SelectValue />
      </SelectTrigger>
      <SelectContent className="font-mono text-[12px]">
        {items.map((item) => (
          <SelectItem key={item.value} value={item.value} className="text-[12px]">
            {item.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function Field({
  id,
  label,
  hint,
  badge,
  children,
}: {
  id?: string;
  label: string;
  hint?: ReactNode;
  badge?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Label htmlFor={id} className="text-[12px] font-medium text-foreground">
          {label}
        </Label>
        {badge}
      </div>
      {children}
      {hint && <p className="font-mono text-[10.5px] leading-snug text-muted-foreground">{hint}</p>}
    </div>
  );
}

function RestartTag() {
  return (
    <span className="rounded-full border border-warning/40 bg-warning/10 px-1.5 py-0.5 font-mono text-[9.5px] text-warning-ink">
      restart
    </span>
  );
}

function ArtifactEditor({
  title,
  sub,
  map,
  inheritedMap,
  disabled,
  onChange,
}: {
  title: string;
  sub: string;
  map: ArtifactMapDraft;
  inheritedMap?: ArtifactMapDraft;
  disabled?: boolean;
  onChange: (arch: ArchKey, patch: Partial<ArtifactDraft>) => void;
}) {
  return (
    <Card title={title} sub={sub}>
      <div className="space-y-4">
        {ARCH_KEYS.map((arch) => {
          const item = map[arch];
          const inh = inheritedMap?.[arch];
          const hasValue = Boolean(item.url || item.sha256 || item.version || item.path);
          return (
            <div key={arch} className="rounded-xl border border-border/80 bg-background/40 p-3.5">
              <div className="mb-3 flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <Badge variant="secondary" className="font-mono text-[10.5px]">
                    {arch}
                  </Badge>
                  {hasValue ? (
                    <span className="font-mono text-[10.5px] text-primary">configured</span>
                  ) : inh && (inh.url || inh.path) ? (
                    <span className="font-mono text-[10.5px] text-muted-foreground">
                      inherited ({inh.version || inh.url || inh.path})
                    </span>
                  ) : (
                    <span className="font-mono text-[10.5px] text-faint">unset</span>
                  )}
                </div>
                {hasValue && !disabled && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="xs"
                    onClick={() => onChange(arch, { url: "", sha256: "", version: "", path: "" })}
                    className="font-mono text-[10.5px] text-muted-foreground hover:text-critical"
                  >
                    Clear {arch}
                  </Button>
                )}
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field id={`${title}-${arch}-version`} label="Version / tag">
                  <Input
                    id={`${title}-${arch}-version`}
                    value={item.version}
                    disabled={disabled}
                    placeholder={inh?.version || "e.g. build-235 or cladefs-6.12"}
                    onChange={(e) => onChange(arch, { version: e.target.value })}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
                <Field id={`${title}-${arch}-sha256`} label="SHA-256 (64 hex chars)">
                  <Input
                    id={`${title}-${arch}-sha256`}
                    value={item.sha256}
                    disabled={disabled}
                    placeholder={inh?.sha256 ? `${inh.sha256.slice(0, 16)}…` : "64-char lowercase hex"}
                    onChange={(e) => onChange(arch, { sha256: e.target.value })}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
                <Field id={`${title}-${arch}-url`} label="Download URL (https://...)">
                  <Input
                    id={`${title}-${arch}-url`}
                    value={item.url}
                    disabled={disabled}
                    placeholder={inh?.url || "https://github.com/.../releases/download/..."}
                    onChange={(e) => onChange(arch, { url: e.target.value })}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
                <Field id={`${title}-${arch}-path`} label="Controller-relative fallback path">
                  <Input
                    id={`${title}-${arch}-path`}
                    value={item.path}
                    disabled={disabled}
                    placeholder={inh?.path || "/builds/N/..."}
                    onChange={(e) => onChange(arch, { path: e.target.value })}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </div>
          );
        })}
      </div>
    </Card>
  );
}

export function SettingsPage() {
  const search = useSearch({ from: "/settings" });
  const navigate = useNavigate();
  const { data: auth } = useAuthMe();
  const { data: snap } = useSnapshot();

  const scope = (search.scope ?? "").trim();
  const isOrgDefaults = scope === "";
  const activeTab: SettingsTab = search.tab && VALID_TABS.includes(search.tab) ? search.tab : "freshen";

  const workosEnabled = Boolean(auth?.workosEnabled);
  const activeOrg = auth?.organization ?? null;
  const isOrgAdmin = !workosEnabled || activeOrg?.role === "admin" || activeOrg?.role === "owner";

  // Fetch all org scopes (defaults + list of node overrides) and, when inspecting
  // a node override, fetch that node's server-computed effective config too.
  const allCfgQuery = useWorkerConfig("");
  const nodeCfgQuery = useWorkerConfig(scope);

  const setConfig = useSetWorkerConfig();
  const deleteConfig = useDeleteWorkerConfig();
  const upgradeSystem = useUpgradeSystem();

  const gitCredsQuery = useGitCredentials();
  const putGitCred = usePutGitCredential();
  const deleteGitCred = useDeleteGitCredential();
  const startGhLogin = useStartGitHubLogin();
  const completeGhLogin = useCompleteGitHubLogin();

  const orgDefaultsCfg: WorkerConfig | undefined = allCfgQuery.data?.defaults;
  const savedNodeOverrides: WorkerConfig[] = allCfgQuery.data?.nodes ?? [];
  const activeScopeCfg: WorkerConfig | undefined = isOrgDefaults
    ? orgDefaultsCfg
    : savedNodeOverrides.find((n) => n.node === scope) ?? nodeCfgQuery.data?.nodes?.[0];
  const effectiveCfg: WorkerConfig | undefined = isOrgDefaults ? orgDefaultsCfg : nodeCfgQuery.data?.effective;

  const tenantId =
    effectiveCfg?.vm?.tenantId ??
    effectiveCfg?.cladefsFuse?.tenantId ??
    orgDefaultsCfg?.vm?.tenantId ??
    orgDefaultsCfg?.cladefsFuse?.tenantId;

  const orgDefaultsDraft = useMemo(() => workerConfigToDraft(orgDefaultsCfg), [orgDefaultsCfg]);
  const baselineDraft = useMemo(() => workerConfigToDraft(activeScopeCfg), [activeScopeCfg]);
  const baselineSig = useMemo(
    () => `${scope}:${String(activeScopeCfg?.revision ?? 0n)}:${JSON.stringify(baselineDraft)}`,
    [scope, activeScopeCfg?.revision, baselineDraft],
  );

  const [draft, setDraft] = useState<SettingsDraft>(() => baselineDraft);
  const [rawJsonText, setRawJsonText] = useState<string>(() => workerConfigToPrettyJson(activeScopeCfg));
  const [rawJsonErr, setRawJsonErr] = useState<string | null>(null);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [saveOk, setSaveOk] = useState<string | null>(null);
  const [customNodeInput, setCustomNodeInput] = useState("");
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [upgradeResults, setUpgradeResults] = useState<NodeSystemUpgrade[] | null>(null);

  // Git credential form state
  const [ghLoginName, setGhLoginName] = useState("github");
  const [ghLoginHost, setGhLoginHost] = useState("github.com");
  const [ghLoginRepos, setGhLoginRepos] = useState("");
  const [ghChallenge, setGhChallenge] = useState<GitHubLoginChallenge | null>(null);

  const [patName, setPatName] = useState("");
  const [patHost, setPatHost] = useState("github.com");
  const [patUser, setPatUser] = useState("");
  const [patRepos, setPatRepos] = useState("");
  const [patApiUrl, setPatApiUrl] = useState("");
  const [patToken, setPatToken] = useState("");

  useEffect(() => {
    setDraft(baselineDraft);
    setRawJsonText(workerConfigToPrettyJson(activeScopeCfg));
    setRawJsonErr(null);
    setSaveErr(null);
  }, [baselineSig]);

  const isDirty = useMemo(() => JSON.stringify(draft) !== JSON.stringify(baselineDraft), [draft, baselineDraft]);
  const needsRestart = useMemo(() => draftRequiresRestart(baselineDraft, draft), [baselineDraft, draft]);

  const knownNodes = useMemo(() => {
    const map = new Map<string, { hostname: string; online: boolean; arch: string; hasOverride: boolean; revision?: bigint }>();
    for (const n of snap?.nodes ?? []) {
      map.set(n.hostname, {
        hostname: n.hostname,
        online: n.online,
        arch: n.resources?.arch ?? "",
        hasOverride: false,
      });
    }
    for (const ov of savedNodeOverrides) {
      if (!ov.node) continue;
      const existing = map.get(ov.node);
      if (existing) {
        existing.hasOverride = true;
        existing.revision = ov.revision;
      } else {
        map.set(ov.node, { hostname: ov.node, online: false, arch: "", hasOverride: true, revision: ov.revision });
      }
    }
    if (scope && !map.has(scope)) {
      map.set(scope, { hostname: scope, online: false, arch: "", hasOverride: false });
    }
    return [...map.values()].sort((a, b) => a.hostname.localeCompare(b.hostname));
  }, [snap?.nodes, savedNodeOverrides, scope]);

  function setScope(nextScope: string) {
    setSaveOk(null);
    setSaveErr(null);
    void navigate({
      to: "/settings",
      search: {
        scope: nextScope || undefined,
        tab: activeTab === "freshen" ? undefined : activeTab,
      },
    });
  }

  function setTab(nextTab: SettingsTab) {
    void navigate({
      to: "/settings",
      search: {
        scope: scope || undefined,
        tab: nextTab === "freshen" ? undefined : nextTab,
      },
    });
  }

  async function copyText(key: string, text: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedKey(key);
      setTimeout(() => setCopiedKey((k) => (k === key ? null : k)), 1800);
    } catch {
      // Clipboard may be unavailable in non-HTTPS contexts.
    }
  }

  async function onSave(e?: FormEvent) {
    e?.preventDefault();
    if (!isOrgAdmin) return;
    setSaveErr(null);
    setSaveOk(null);
    try {
      const config = draftToWorkerConfigInput(draft, { isOrgDefaults });
      const saved = await setConfig.mutateAsync({
        node: scope,
        config,
        replace: true,
        ifRevision: activeScopeCfg?.revision ?? 0n,
      });
      setRawJsonText(workerConfigToPrettyJson(saved));
      setSaveOk(
        isOrgDefaults
          ? `Saved organization defaults (rev ${saved.revision}).`
          : `Saved override for ${scope} (rev ${saved.revision}).`,
      );
    } catch (err) {
      setSaveErr(err instanceof Error ? err.message : String(err));
    }
  }

  async function onSaveRawJson() {
    if (!isOrgAdmin) return;
    setRawJsonErr(null);
    setSaveErr(null);
    setSaveOk(null);
    try {
      const parsed = parseWorkerConfigJson(rawJsonText);
      const saved = await setConfig.mutateAsync({
        node: scope,
        config: parsed,
        replace: true,
        ifRevision: activeScopeCfg?.revision ?? 0n,
      });
      setDraft(workerConfigToDraft(saved));
      setRawJsonText(workerConfigToPrettyJson(saved));
      setSaveOk(`Saved raw JSON (rev ${saved.revision}).`);
    } catch (err) {
      setRawJsonErr(err instanceof Error ? err.message : String(err));
    }
  }

  async function onResetScope() {
    if (!isOrgAdmin) return;
    setSaveErr(null);
    setSaveOk(null);
    try {
      await deleteConfig.mutateAsync(isOrgDefaults ? { defaults: true } : { node: scope });
      setSaveOk(
        isOrgDefaults
          ? "Cleared organization defaults (all nodes now use built-in defaults)."
          : `Removed overrides for ${scope} (now inherits organization defaults).`,
      );
    } catch (err) {
      setSaveErr(err instanceof Error ? err.message : String(err));
    }
  }

  async function onTriggerUpgrade(targetNode: string) {
    if (!isOrgAdmin) return;
    setUpgradeResults(null);
    try {
      const res = await upgradeSystem.mutateAsync(targetNode);
      setUpgradeResults(res.nodes);
    } catch {
      // Error is exposed on upgradeSystem.error
    }
  }

  async function onStartGitHub(e: FormEvent) {
    e.preventDefault();
    if (!isOrgAdmin) return;
    setGhChallenge(null);
    try {
      const repos = ghLoginRepos
        .split(/[,\n]/)
        .map((r) => r.trim())
        .filter(Boolean);
      const ch = await startGhLogin.mutateAsync({
        name: ghLoginName.trim() || "github",
        host: ghLoginHost.trim() || "github.com",
        repos,
      });
      setGhChallenge(ch);
    } catch {
      // Exposed on startGhLogin.error
    }
  }

  async function onCompleteGitHub() {
    if (!ghChallenge || !isOrgAdmin) return;
    try {
      await completeGhLogin.mutateAsync(ghChallenge.loginId);
      setGhChallenge(null);
    } catch {
      // Exposed on completeGhLogin.error
    }
  }

  async function onSavePat(e: FormEvent) {
    e.preventDefault();
    if (!isOrgAdmin || !patName.trim() || !patToken.trim()) return;
    try {
      const repos = patRepos
        .split(/[,\n]/)
        .map((r) => r.trim())
        .filter(Boolean);
      await putGitCred.mutateAsync({
        name: patName.trim(),
        host: patHost.trim() || "github.com",
        username: patUser.trim(),
        repos,
        apiUrl: patApiUrl.trim(),
        token: patToken.trim(),
      });
      setPatName("");
      setPatToken("");
      setPatRepos("");
      setPatApiUrl("");
    } catch {
      // Exposed on putGitCred.error
    }
  }

  const cliPreview = useMemo(() => {
    try {
      const input = draftToWorkerConfigInput(draft, { isOrgDefaults });
      return draftToCliCommand(scope, input);
    } catch {
      return `# Fix validation errors in the form to preview the CLI command`;
    }
  }, [draft, isOrgDefaults, scope]);

  const dutyPreview = useMemo(() => {
    const raw = draft.freshen.maxDutyCyclePercent.trim();
    const inheritedPct = orgDefaultsDraft.freshen.maxDutyCyclePercent.trim();
    const pct = Number(raw || (!isOrgDefaults ? inheritedPct : "") || BUILTIN_DEFAULTS.freshen.maxDutyCyclePercent);
    if (!Number.isFinite(pct) || pct <= 0 || pct > 100) return "valid range: (0, 100]%";
    const coolPerMin = 100 / pct - 1;
    return `${pct}% cap · 1m active freshen → ${coolPerMin >= 1 ? `${coolPerMin.toFixed(coolPerMin < 10 ? 1 : 0)}m` : `${Math.round(coolPerMin * 60)}s`} cooldown`;
  }, [draft.freshen.maxDutyCyclePercent, orgDefaultsDraft.freshen.maxDutyCyclePercent, isOrgDefaults]);

  const nowMs = Number(snap?.now ?? Date.now());
  const orgLabel = activeOrg?.name ?? "Organization";
  const loadingCfg = allCfgQuery.isLoading || (!isOrgDefaults && nodeCfgQuery.isLoading);
  const cfgLoadErr = allCfgQuery.error ?? (!isOrgDefaults ? nodeCfgQuery.error : null);

  return (
    <div className="space-y-6">
      <PageTitle
        title={activeOrg ? `${activeOrg.name} Settings` : "Organization Settings"}
        sub={
          activeOrg
            ? `Scoped exclusively to ${activeOrg.name} (${activeOrg.id}) · isolated Controller DO · cluster ${snap?.clusterId ?? "…"} · never shared across organizations`
            : `Per-organization configuration for cluster ${snap?.clusterId ?? "…"} · defaults apply to every node in this organization unless overridden per node`
        }
        right={
          <div className="flex flex-wrap items-center gap-2">
            {tenantId !== undefined && (
              <Badge variant="outline" className="border-border bg-card/60 font-mono text-[10.5px] text-muted-foreground">
                cladefs tenant {String(tenantId)}
              </Badge>
            )}
            <Badge
              variant="secondary"
              className={
                isOrgAdmin
                  ? "bg-ember-wash font-mono text-[10.5px] text-primary"
                  : "bg-muted font-mono text-[10.5px] text-muted-foreground"
              }
            >
              {isOrgAdmin ? (activeOrg?.role ? `${activeOrg.role} · editable` : "editable") : `${activeOrg?.role ?? "member"} · read-only`}
            </Badge>
          </div>
        }
      />

      {!isOrgAdmin && (
        <Alert className="rounded-xl border-border-strong bg-card/80">
          <ShieldAlert className="size-4 text-muted-foreground" />
          <AlertTitle className="text-[12.5px] font-medium">Read-only organization access</AlertTitle>
          <AlertDescription className="text-[12px] text-muted-foreground">
            You are signed in as <span className="font-mono text-foreground">{activeOrg?.role ?? "member"}</span> in{" "}
            <span className="font-medium text-foreground">{orgLabel}</span>. Only organization admins can modify
            settings, node overrides, host upgrades, or git credentials.
          </AlertDescription>
        </Alert>
      )}

      {/* Scope bar: Org defaults vs per-node overrides */}
      <Card
        title="Configuration Scope"
        sub={`Changes here apply only to ${orgLabel}. Choose Organization defaults to configure all nodes in this org, or pick a node to set per-node overrides.`}
        right={
          activeScopeCfg && activeScopeCfg.revision > 0n ? (
            <div className="flex flex-wrap items-center gap-2 font-mono text-[11px] text-muted-foreground">
              <span>rev {String(activeScopeCfg.revision)}</span>
              {activeScopeCfg.updatedAt > 0n && <span>· {ago(activeScopeCfg.updatedAt, nowMs)}</span>}
              {activeScopeCfg.updatedBy && <span>· by {activeScopeCfg.updatedBy}</span>}
            </div>
          ) : (
            <span className="font-mono text-[11px] text-faint">
              {isOrgDefaults ? "using built-in defaults" : "inheriting org defaults"}
            </span>
          )
        }
      >
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            variant={isOrgDefaults ? "default" : "outline"}
            size="sm"
            onClick={() => setScope("")}
            className="gap-1.5 font-mono text-[11.5px]"
          >
            <SlidersHorizontal className="size-3.5" />
            Org defaults
            {orgDefaultsCfg && orgDefaultsCfg.revision > 0n && (
              <span className="ml-0.5 rounded bg-background/20 px-1 text-[10px]">r{String(orgDefaultsCfg.revision)}</span>
            )}
          </Button>

          {knownNodes.map((n) => {
            const selected = scope === n.hostname;
            return (
              <Button
                key={n.hostname}
                type="button"
                variant={selected ? "default" : "outline"}
                size="sm"
                onClick={() => setScope(n.hostname)}
                className="gap-1.5 font-mono text-[11.5px]"
              >
                <span
                  className={`size-1.5 rounded-full ${
                    n.online ? (selected ? "bg-primary-foreground" : "bg-primary") : "bg-muted-foreground/50"
                  }`}
                />
                {n.hostname}
                {n.hasOverride && (
                  <span
                    className={`rounded px-1 text-[9.5px] ${
                      selected ? "bg-background/20 text-primary-foreground" : "bg-ember-wash text-primary"
                    }`}
                  >
                    override
                  </span>
                )}
              </Button>
            );
          })}

          {isOrgAdmin && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const name = customNodeInput.trim();
                if (!name) return;
                setCustomNodeInput("");
                setScope(name);
              }}
              className="ml-auto flex items-center gap-1.5"
            >
              <Input
                value={customNodeInput}
                onChange={(e) => setCustomNodeInput(e.target.value)}
                placeholder="Add node hostname…"
                className="h-8 w-40 bg-background font-mono text-[11.5px]"
              />
              <Button type="submit" variant="outline" size="sm" disabled={!customNodeInput.trim()} className="font-mono text-[11px]">
                + Node
              </Button>
            </form>
          )}
        </div>
      </Card>

      {cfgLoadErr && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 font-mono text-[11.5px]">
          <AlertDescription className="text-[11.5px] text-critical">
            Failed to load organization config: {String(cfgLoadErr)}
          </AlertDescription>
        </Alert>
      )}

      {saveErr && (
        <Alert variant="destructive" className="rounded-xl border-critical/40 font-mono text-[11.5px]">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <AlertDescription className="text-[11.5px] text-critical">{saveErr}</AlertDescription>
            <Button
              type="button"
              variant="outline"
              size="xs"
              onClick={() => {
                void allCfgQuery.refetch();
                if (!isOrgDefaults) void nodeCfgQuery.refetch();
              }}
              className="font-mono text-[10.5px]"
            >
              <RefreshCw className="mr-1 size-3" /> Reload latest
            </Button>
          </div>
        </Alert>
      )}

      {saveOk && (
        <Alert className="rounded-xl border-primary/40 bg-ember-wash/60 font-mono text-[11.5px]">
          <Check className="size-4 text-primary" />
          <AlertDescription className="text-[11.5px] text-foreground">{saveOk}</AlertDescription>
        </Alert>
      )}

      {/* Save bar for WorkerConfig tabs */}
      {activeTab !== "git" && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-2xl border border-border bg-card/90 px-4 py-3 backdrop-blur-sm">
          <div className="flex flex-wrap items-center gap-2 text-[12px]">
            <span className="font-mono font-medium text-foreground">
              {isOrgDefaults ? `Editing ${orgLabel} defaults` : `Editing node override: ${scope}`}
            </span>
            {isDirty ? (
              <Badge variant="outline" className="border-primary/50 bg-ember-wash font-mono text-[10px] text-primary">
                unsaved changes
              </Badge>
            ) : (
              <Badge variant="secondary" className="font-mono text-[10px] text-muted-foreground">
                in sync
              </Badge>
            )}
            {isDirty && (
              <span
                className={`font-mono text-[10.5px] ${needsRestart ? "text-warning-ink" : "text-good-ink"}`}
              >
                {needsRestart
                  ? "· structural change (idle-deferred worker restart)"
                  : "· hot-reloads live on next heartbeat (no restart)"}
              </span>
            )}
          </div>

          {isOrgAdmin && (
            <div className="flex flex-wrap items-center gap-2">
              {activeScopeCfg && activeScopeCfg.revision > 0n && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={deleteConfig.isPending || setConfig.isPending}
                  onClick={() => void onResetScope()}
                  className="gap-1.5 font-mono text-[11px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                >
                  <Trash2 className="size-3.5" />
                  {isOrgDefaults ? "Reset org defaults" : `Remove ${scope} override`}
                </Button>
              )}
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={!isDirty || setConfig.isPending}
                onClick={() => {
                  setDraft(baselineDraft);
                  setRawJsonText(workerConfigToPrettyJson(activeScopeCfg));
                  setSaveErr(null);
                  setRawJsonErr(null);
                }}
                className="gap-1.5 font-mono text-[11px]"
              >
                <RotateCcw className="size-3.5" />
                Discard
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={!isDirty || setConfig.isPending}
                onClick={() => void onSave()}
                className="gap-1.5 font-mono text-[11.5px]"
              >
                <Save className="size-3.5" />
                {setConfig.isPending ? "Saving…" : "Save changes"}
              </Button>
            </div>
          )}
        </div>
      )}

      {loadingCfg ? (
        <div className="py-12 text-center font-mono text-[12px] text-muted-foreground">loading organization settings…</div>
      ) : (
        <Tabs value={activeTab} onValueChange={(v) => v && setTab(v as SettingsTab)} className="space-y-5">
          <TabsList className="h-auto w-full flex-wrap justify-start gap-1 rounded-xl border border-border bg-card/70 p-1">
            <TabsTrigger value="freshen" className="px-3 py-1.5 font-mono text-[11.5px]">
              Base Freshening
            </TabsTrigger>
            <TabsTrigger value="runtime" className="px-3 py-1.5 font-mono text-[11.5px]">
              Runtime
            </TabsTrigger>
            <TabsTrigger value="vm" className="px-3 py-1.5 font-mono text-[11.5px]">
              Firecracker VM
            </TabsTrigger>
            <TabsTrigger value="fuse" className="px-3 py-1.5 font-mono text-[11.5px]">
              CladeFS FUSE
            </TabsTrigger>
            <TabsTrigger value="system" className="px-3 py-1.5 font-mono text-[11.5px]">
              System & Upgrades
            </TabsTrigger>
            <TabsTrigger value="git" className="px-3 py-1.5 font-mono text-[11.5px]">
              Git Auth
            </TabsTrigger>
            <TabsTrigger value="json" className="px-3 py-1.5 font-mono text-[11.5px]">
              Raw JSON & CLI
            </TabsTrigger>
          </TabsList>

          {/* 1. BASE FRESHENING + CONTROLLER LIFECYCLE */}
          <TabsContent value="freshen" className="space-y-5">
            <Card
              title="Base Freshening Caps"
              sub="Bounds commit-only base freshening so it stays a lightweight optimization. Structural changes (cold builds, setup step edits, project rebuilds) always run immediately. Hot-reloads with zero restart."
              right={
                isOrgAdmin && (
                  <div className="flex flex-wrap gap-1.5">
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          freshen: {
                            disableOptional: "false",
                            maxDutyCyclePercent: "5",
                            minIntervalSeconds: "1800",
                            maxCooldownSeconds: "21600",
                            failureCooldownSeconds: "900",
                            maxOptionalDurationSeconds: "1800",
                            maxRequiredDurationSeconds: "3600",
                            idleDelaySeconds: "15",
                          },
                        }))
                      }
                      className="font-mono text-[10.5px]"
                    >
                      Balanced (5% / 30m)
                    </Button>
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          freshen: {
                            ...d.freshen,
                            disableOptional: "false",
                            maxDutyCyclePercent: "2",
                            minIntervalSeconds: "3600",
                            maxCooldownSeconds: "21600",
                            idleDelaySeconds: "60",
                          },
                        }))
                      }
                      className="font-mono text-[10.5px]"
                    >
                      Conservative (2% / 60m)
                    </Button>
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          freshen: { ...d.freshen, disableOptional: "true" },
                        }))
                      }
                      className="font-mono text-[10.5px]"
                    >
                      Pause optional
                    </Button>
                  </div>
                )
              }
            >
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field
                  id="freshen-disable-optional"
                  label="Optional commit-only freshens"
                  hint="When paused, nodes only freshen on cold start, step/repo edits, or `hotbed project rebuild`."
                >
                  <TriSelect
                    id="freshen-disable-optional"
                    value={draft.freshen.disableOptional}
                    disabled={!isOrgAdmin}
                    inheritLabel={
                      !isOrgDefaults && orgDefaultsCfg?.freshen?.disableOptional !== undefined
                        ? orgDefaultsCfg.freshen.disableOptional
                          ? "Inherit (paused)"
                          : "Inherit (active)"
                        : "Default (active)"
                    }
                    trueLabel="Paused (disable optional)"
                    falseLabel="Active (capped)"
                    onChange={(v) => setDraft((d) => ({ ...d, freshen: { ...d.freshen, disableOptional: v } }))}
                  />
                </Field>

                <Field id="freshen-duty" label="Max duty cycle (%)" hint={dutyPreview}>
                  <Input
                    id="freshen-duty"
                    type="number"
                    step="0.5"
                    min="0.1"
                    max="100"
                    value={draft.freshen.maxDutyCyclePercent}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.maxDutyCyclePercent
                        ? `Inherit (${orgDefaultsDraft.freshen.maxDutyCyclePercent}%)`
                        : `${BUILTIN_DEFAULTS.freshen.maxDutyCyclePercent}`
                    }
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, freshen: { ...d.freshen, maxDutyCyclePercent: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-min-interval"
                  label="Min interval per project (s)"
                  hint={formatSecondsHint(
                    draft.freshen.minIntervalSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.minIntervalSeconds !== undefined
                      ? orgDefaultsCfg.freshen.minIntervalSeconds
                      : BUILTIN_DEFAULTS.freshen.minIntervalSeconds,
                  )}
                >
                  <Input
                    id="freshen-min-interval"
                    type="number"
                    min="0"
                    value={draft.freshen.minIntervalSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.minIntervalSeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.minIntervalSeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.minIntervalSeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, freshen: { ...d.freshen, minIntervalSeconds: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-max-cooldown"
                  label="Max cooldown cap (s)"
                  hint={formatSecondsHint(
                    draft.freshen.maxCooldownSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.maxCooldownSeconds !== undefined
                      ? orgDefaultsCfg.freshen.maxCooldownSeconds
                      : BUILTIN_DEFAULTS.freshen.maxCooldownSeconds,
                  )}
                >
                  <Input
                    id="freshen-max-cooldown"
                    type="number"
                    min="0"
                    value={draft.freshen.maxCooldownSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.maxCooldownSeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.maxCooldownSeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.maxCooldownSeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, freshen: { ...d.freshen, maxCooldownSeconds: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-failure-backoff"
                  label="Failure exponential backoff (s)"
                  hint={`${formatSecondsHint(
                    draft.freshen.failureCooldownSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.failureCooldownSeconds !== undefined
                      ? orgDefaultsCfg.freshen.failureCooldownSeconds
                      : BUILTIN_DEFAULTS.freshen.failureCooldownSeconds,
                  )} · doubles per failure`}
                >
                  <Input
                    id="freshen-failure-backoff"
                    type="number"
                    min="0"
                    value={draft.freshen.failureCooldownSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.failureCooldownSeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.failureCooldownSeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.failureCooldownSeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, freshen: { ...d.freshen, failureCooldownSeconds: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-idle-delay"
                  label="Idle settle delay (s)"
                  hint={`${formatSecondsHint(
                    draft.freshen.idleDelaySeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.idleDelaySeconds !== undefined
                      ? orgDefaultsCfg.freshen.idleDelaySeconds
                      : BUILTIN_DEFAULTS.freshen.idleDelaySeconds,
                  )} quiet before background (-10)`}
                >
                  <Input
                    id="freshen-idle-delay"
                    type="number"
                    min="0"
                    value={draft.freshen.idleDelaySeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.idleDelaySeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.idleDelaySeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.idleDelaySeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, freshen: { ...d.freshen, idleDelaySeconds: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-opt-limit"
                  label="Optional hard limit (s)"
                  hint={`${formatSecondsHint(
                    draft.freshen.maxOptionalDurationSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.maxOptionalDurationSeconds !== undefined
                      ? orgDefaultsCfg.freshen.maxOptionalDurationSeconds
                      : BUILTIN_DEFAULTS.freshen.maxOptionalDurationSeconds,
                  )} · 0 = unlimited`}
                >
                  <Input
                    id="freshen-opt-limit"
                    type="number"
                    min="0"
                    value={draft.freshen.maxOptionalDurationSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.maxOptionalDurationSeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.maxOptionalDurationSeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.maxOptionalDurationSeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({
                        ...d,
                        freshen: { ...d.freshen, maxOptionalDurationSeconds: e.target.value },
                      }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="freshen-req-limit"
                  label="Required hard limit (s)"
                  hint={`${formatSecondsHint(
                    draft.freshen.maxRequiredDurationSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.freshen?.maxRequiredDurationSeconds !== undefined
                      ? orgDefaultsCfg.freshen.maxRequiredDurationSeconds
                      : BUILTIN_DEFAULTS.freshen.maxRequiredDurationSeconds,
                  )} · cold/setup/rebuild`}
                >
                  <Input
                    id="freshen-req-limit"
                    type="number"
                    min="0"
                    value={draft.freshen.maxRequiredDurationSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.freshen.maxRequiredDurationSeconds
                        ? `Inherit (${orgDefaultsDraft.freshen.maxRequiredDurationSeconds}s)`
                        : String(BUILTIN_DEFAULTS.freshen.maxRequiredDurationSeconds)
                    }
                    onChange={(e) =>
                      setDraft((d) => ({
                        ...d,
                        freshen: { ...d.freshen, maxRequiredDurationSeconds: e.target.value },
                      }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>

            <Card
              title="Controller & Session Lifecycle"
              sub="Per-organization session reaping timeouts enforced by this organization's Controller Durable Object."
              right={
                !isOrgDefaults && (
                  <Button
                    type="button"
                    variant="outline"
                    size="xs"
                    onClick={() => setScope("")}
                    className="font-mono text-[10.5px]"
                  >
                    Edit in Org defaults →
                  </Button>
                )
              }
            >
              {isOrgDefaults ? (
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field
                    id="ctrl-session-idle"
                    label="Session idle timeout (s)"
                    hint={`${formatSecondsHint(
                      draft.controller.sessionIdleTimeoutSeconds,
                      BUILTIN_DEFAULTS.controller.sessionIdleTimeoutSeconds,
                    )} · minimum 60s`}
                  >
                    <Input
                      id="ctrl-session-idle"
                      type="number"
                      min="60"
                      value={draft.controller.sessionIdleTimeoutSeconds}
                      disabled={!isOrgAdmin}
                      placeholder={String(BUILTIN_DEFAULTS.controller.sessionIdleTimeoutSeconds)}
                      onChange={(e) =>
                        setDraft((d) => ({
                          ...d,
                          controller: { ...d.controller, sessionIdleTimeoutSeconds: e.target.value },
                        }))
                      }
                      className="bg-background font-mono text-[12px]"
                    />
                  </Field>

                  <Field
                    id="ctrl-offline-grace"
                    label="Offline node grace period (s)"
                    hint={`${formatSecondsHint(
                      draft.controller.offlineGraceSeconds,
                      BUILTIN_DEFAULTS.controller.offlineGraceSeconds,
                    )} · wait before reaping sessions on an offline node`}
                  >
                    <Input
                      id="ctrl-offline-grace"
                      type="number"
                      min="0"
                      value={draft.controller.offlineGraceSeconds}
                      disabled={!isOrgAdmin}
                      placeholder={String(BUILTIN_DEFAULTS.controller.offlineGraceSeconds)}
                      onChange={(e) =>
                        setDraft((d) => ({
                          ...d,
                          controller: { ...d.controller, offlineGraceSeconds: e.target.value },
                        }))
                      }
                      className="bg-background font-mono text-[12px]"
                    />
                  </Field>
                </div>
              ) : (
                <p className="font-mono text-[11.5px] text-muted-foreground">
                  Applies to the organization as a whole (currently{" "}
                  <span className="text-foreground">
                    {formatDurationSeconds(
                      orgDefaultsCfg?.controller?.sessionIdleTimeoutSeconds ??
                        BUILTIN_DEFAULTS.controller.sessionIdleTimeoutSeconds,
                    )}
                  </span>{" "}
                  session idle timeout ·{" "}
                  <span className="text-foreground">
                    {formatDurationSeconds(
                      orgDefaultsCfg?.controller?.offlineGraceSeconds ?? BUILTIN_DEFAULTS.controller.offlineGraceSeconds,
                    )}
                  </span>{" "}
                  offline grace). Switch scope to Org defaults to change.
                </p>
              )}
            </Card>
          </TabsContent>

          {/* 2. RUNTIME & SYSTEMD */}
          <TabsContent value="runtime" className="space-y-5">
            <Card
              title="Runtime & Systemd Defaults"
              sub="Controls which workspace runtime non-bench runs select by default and how worker systemd units are configured."
            >
              <div className="grid gap-4 sm:grid-cols-2">
                <Field
                  id="rt-default"
                  label="Default workspace runtime"
                  hint={
                    !isOrgDefaults && orgDefaultsCfg?.defaultRuntime
                      ? `Org default: ${orgDefaultsCfg.defaultRuntime}`
                      : "Used for non-bench workspaces when --runtime is omitted"
                  }
                >
                  <Select
                    items={RUNTIME_ITEMS}
                    value={draft.defaultRuntime || "__inherit__"}
                    disabled={!isOrgAdmin}
                    onValueChange={(v) =>
                      setDraft((d) => ({
                        ...d,
                        defaultRuntime: v === "vm" || v === "cladefs" ? v : "",
                      }))
                    }
                  >
                    <SelectTrigger id="rt-default" className="w-full bg-background font-mono text-[12px]">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent className="font-mono text-[12px]">
                      {RUNTIME_ITEMS.map((item) => (
                        <SelectItem key={item.value} value={item.value} className="text-[12px]">
                          {item.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>

                <Field
                  id="rt-slice"
                  label="Systemd slice"
                  badge={<RestartTag />}
                  hint={
                    !isOrgDefaults && orgDefaultsCfg?.slice
                      ? `Org default: ${orgDefaultsCfg.slice}`
                      : "e.g. hotbed.slice or user-1000-hotbed.slice"
                  }
                >
                  <Input
                    id="rt-slice"
                    value={draft.slice}
                    disabled={!isOrgAdmin}
                    placeholder={!isOrgDefaults && orgDefaultsCfg?.slice ? orgDefaultsCfg.slice : "hotbed.slice"}
                    onChange={(e) => setDraft((d) => ({ ...d, slice: e.target.value }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>

              <div className="mt-4">
                <Field
                  id="rt-unit-props"
                  label="Systemd unit properties"
                  badge={<RestartTag />}
                  hint="One Key=Value property per line (replaces lower layer when non-empty)."
                >
                  <Textarea
                    id="rt-unit-props"
                    rows={4}
                    value={draft.unitPropertiesText}
                    disabled={!isOrgAdmin}
                    placeholder={"TasksMax=4096\nLimitNOFILE=65536"}
                    onChange={(e) => setDraft((d) => ({ ...d, unitPropertiesText: e.target.value }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>
          </TabsContent>

          {/* 3. FIRECRACKER VM */}
          <TabsContent value="vm" className="space-y-5">
            <Card
              title="Firecracker VM Compute & Lifecycle"
              sub="Sizing and idle hibernation for microVM workspaces. Sizing changes hot-reload for newly started VMs; enabling/disabling VM mode triggers an idle-deferred restart."
              right={
                tenantId !== undefined && (
                  <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
                    managed tenant_id = {String(tenantId)}
                  </Badge>
                )
              }
            >
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field id="vm-enabled" label="VM runtime enabled" badge={<RestartTag />}>
                  <TriSelect
                    id="vm-enabled"
                    value={draft.vm.enabled}
                    disabled={!isOrgAdmin}
                    inheritLabel={inheritedTriLabel(!isOrgDefaults ? orgDefaultsCfg?.vm?.enabled : undefined, false)}
                    onChange={(v) => setDraft((d) => ({ ...d, vm: { ...d.vm, enabled: v } }))}
                  />
                </Field>

                <Field id="vm-default-rt" label="Use VM as default runtime">
                  <TriSelect
                    id="vm-default-rt"
                    value={draft.vm.defaultRuntime}
                    disabled={!isOrgAdmin}
                    inheritLabel={inheritedTriLabel(
                      !isOrgDefaults ? orgDefaultsCfg?.vm?.defaultRuntime : undefined,
                      false,
                    )}
                    onChange={(v) => setDraft((d) => ({ ...d, vm: { ...d.vm, defaultRuntime: v } }))}
                  />
                </Field>

                <Field
                  id="vm-vcpus"
                  label="vCPUs per VM"
                  hint={!isOrgDefaults && orgDefaultsCfg?.vm?.vcpus ? `Org default: ${orgDefaultsCfg.vm.vcpus}` : "0 = worker default"}
                >
                  <Input
                    id="vm-vcpus"
                    type="number"
                    min="0"
                    value={draft.vm.vcpus}
                    disabled={!isOrgAdmin}
                    placeholder={!isOrgDefaults && orgDefaultsDraft.vm.vcpus ? orgDefaultsDraft.vm.vcpus : "8"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, vcpus: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="vm-mem"
                  label="Memory per VM (MiB)"
                  hint={
                    draft.vm.memMib.trim() && Number(draft.vm.memMib) > 0
                      ? `${(Number(draft.vm.memMib) / 1024).toFixed(1)} GiB`
                      : !isOrgDefaults && orgDefaultsCfg?.vm?.memMib
                        ? `Org default: ${orgDefaultsCfg.vm.memMib} MiB`
                        : "e.g. 8192 or 12288"
                  }
                >
                  <Input
                    id="vm-mem"
                    type="number"
                    min="0"
                    value={draft.vm.memMib}
                    disabled={!isOrgAdmin}
                    placeholder={!isOrgDefaults && orgDefaultsDraft.vm.memMib ? orgDefaultsDraft.vm.memMib : "8192"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, memMib: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="vm-idle-suspend"
                  label="Idle VM suspend timeout (s)"
                  hint={`${formatSecondsHint(
                    draft.vm.idleSuspendSeconds,
                    !isOrgDefaults && orgDefaultsCfg?.vm?.idleSuspendSeconds !== undefined
                      ? orgDefaultsCfg.vm.idleSuspendSeconds
                      : BUILTIN_DEFAULTS.vm.idleSuspendSeconds,
                  )} · 0 disables suspend`}
                >
                  <Input
                    id="vm-idle-suspend"
                    type="number"
                    min="0"
                    value={draft.vm.idleSuspendSeconds}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.vm.idleSuspendSeconds
                        ? `Inherit (${orgDefaultsDraft.vm.idleSuspendSeconds}s)`
                        : String(BUILTIN_DEFAULTS.vm.idleSuspendSeconds)
                    }
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, idleSuspendSeconds: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="vm-nofile"
                  label="Guest RLIMIT_NOFILE"
                  hint={`default: ${BUILTIN_DEFAULTS.vm.nofile}`}
                >
                  <Input
                    id="vm-nofile"
                    type="number"
                    min="0"
                    value={draft.vm.nofile}
                    disabled={!isOrgAdmin}
                    placeholder={
                      !isOrgDefaults && orgDefaultsDraft.vm.nofile
                        ? orgDefaultsDraft.vm.nofile
                        : String(BUILTIN_DEFAULTS.vm.nofile)
                    }
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, nofile: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-disk" label="Rootfs disk size (bytes)" hint={formatBytesHint(draft.vm.diskBytes)}>
                  <Input
                    id="vm-disk"
                    value={draft.vm.diskBytes}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.diskBytes || "e.g. 68719476736 (64 GiB)"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, diskBytes: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-cache-type" label="virtio-extent cache type">
                  <Select
                    items={CACHE_TYPE_ITEMS}
                    value={draft.vm.cacheType || "__inherit__"}
                    disabled={!isOrgAdmin}
                    onValueChange={(v) =>
                      setDraft((d) => ({
                        ...d,
                        vm: { ...d.vm, cacheType: v === "Unsafe" || v === "Writeback" ? v : "" },
                      }))
                    }
                  >
                    <SelectTrigger id="vm-cache-type" className="w-full bg-background font-mono text-[12px]">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent className="font-mono text-[12px]">
                      {CACHE_TYPE_ITEMS.map((item) => (
                        <SelectItem key={item.value} value={item.value} className="text-[12px]">
                          {item.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>

                <Field id="vm-disable-dax" label="Disable DAX window" hint="Needed on 16 KiB Apple Silicon hosts.">
                  <TriSelect
                    id="vm-disable-dax"
                    value={draft.vm.disableDax}
                    disabled={!isOrgAdmin}
                    inheritLabel={inheritedTriLabel(!isOrgDefaults ? orgDefaultsCfg?.vm?.disableDax : undefined, false)}
                    trueLabel="Disable DAX"
                    falseLabel="Allow DAX"
                    onChange={(v) => setDraft((d) => ({ ...d, vm: { ...d.vm, disableDax: v } }))}
                  />
                </Field>

                <Field id="vm-dax-window" label="DAX window (bytes)" hint={formatBytesHint(draft.vm.daxWindowBytes)}>
                  <Input
                    id="vm-dax-window"
                    value={draft.vm.daxWindowBytes}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.daxWindowBytes || "e.g. 17179869184 (16 GiB)"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, daxWindowBytes: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field
                  id="vm-dax-private"
                  label="DAX private max (bytes)"
                  hint={formatBytesHint(draft.vm.daxPrivateMaxBytes)}
                >
                  <Input
                    id="vm-dax-private"
                    value={draft.vm.daxPrivateMaxBytes}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.daxPrivateMaxBytes || "bytes"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, daxPrivateMaxBytes: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-user" label="VM command user">
                  <Input
                    id="vm-user"
                    value={draft.vm.vmUser}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.vmUser || "hotbed"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, vmUser: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>

            <Card
              title="VM Networking, Sockets & Host Paths"
              sub="Low-level bridge, CladeFS daemon socket, and host binary paths. Usually left to local hotbed.json unless overridden per node."
            >
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field id="vm-bridge" label="Bridge name" badge={<RestartTag />}>
                  <Input
                    id="vm-bridge"
                    value={draft.vm.bridgeName}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.bridgeName || "hotbed0"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, bridgeName: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-cidr" label="Bridge CIDR" badge={<RestartTag />}>
                  <Input
                    id="vm-cidr"
                    value={draft.vm.bridgeCidr}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.bridgeCidr || "10.200.0.1/24"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, bridgeCidr: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-dns" label="DNS servers (comma-separated)">
                  <Input
                    id="vm-dns"
                    value={draft.vm.dnsServersText}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.dnsServersText || "1.1.1.1, 8.8.8.8"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, dnsServersText: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-peer-addr" label="CladeFS peer address">
                  <Input
                    id="vm-peer-addr"
                    value={draft.vm.cladefsPeerAddr}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.cladefsPeerAddr || "100.x.y.z:9185"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, cladefsPeerAddr: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-ctrl-sock" label="CladeFS control socket" badge={<RestartTag />}>
                  <Input
                    id="vm-ctrl-sock"
                    value={draft.vm.cladefsControlSock}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.cladefsControlSock || "/run/cladefsd/control.sock"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, cladefsControlSock: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-vm-sock" label="CladeFS VM socket" badge={<RestartTag />}>
                  <Input
                    id="vm-vm-sock"
                    value={draft.vm.cladefsVmSock}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.cladefsVmSock || "/run/cladefsd/vm.sock"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, cladefsVmSock: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-cell-root" label="Cell root" badge={<RestartTag />}>
                  <Input
                    id="vm-cell-root"
                    value={draft.vm.cellRoot}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.cellRoot || "/var/lib/cladefs/cells"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, cellRoot: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-fc-bin" label="Firecracker binary path">
                  <Input
                    id="vm-fc-bin"
                    value={draft.vm.firecrackerBin}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.firecrackerBin || "/usr/local/bin/firecracker"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, firecrackerBin: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-kernel-path" label="Local kernel path">
                  <Input
                    id="vm-kernel-path"
                    value={draft.vm.kernelPath}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.kernelPath || "/var/lib/hotbed/vmlinux"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, kernelPath: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="vm-guestd-bin" label="Local guestd path">
                  <Input
                    id="vm-guestd-bin"
                    value={draft.vm.guestdBin}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.vm.guestdBin || "/usr/local/bin/hotbed-guestd"}
                    onChange={(e) => setDraft((d) => ({ ...d, vm: { ...d.vm, guestdBin: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>

            <div className="grid gap-5 xl:grid-cols-2">
              <ArtifactEditor
                title="Managed Guest Kernels (vmlinux)"
                sub="SHA-256 verified guest kernels downloaded automatically by workers per architecture."
                map={draft.vm.kernels}
                inheritedMap={!isOrgDefaults ? orgDefaultsDraft.vm.kernels : undefined}
                disabled={!isOrgAdmin}
                onChange={(arch, patch) =>
                  setDraft((d) => ({
                    ...d,
                    vm: { ...d.vm, kernels: { ...d.vm.kernels, [arch]: { ...d.vm.kernels[arch], ...patch } } },
                  }))
                }
              />
              <ArtifactEditor
                title="Managed hotbed-guestd Overrides"
                sub="Optional per-arch guestd override. Leave unset to follow each controller release automatically."
                map={draft.vm.guestd}
                inheritedMap={!isOrgDefaults ? orgDefaultsDraft.vm.guestd : undefined}
                disabled={!isOrgAdmin}
                onChange={(arch, patch) =>
                  setDraft((d) => ({
                    ...d,
                    vm: { ...d.vm, guestd: { ...d.vm.guestd, [arch]: { ...d.vm.guestd[arch], ...patch } } },
                  }))
                }
              />
            </div>
          </TabsContent>

          {/* 4. CLADEFS FUSE */}
          <TabsContent value="fuse" className="space-y-5">
            <Card
              title="CladeFS Host-FUSE Runtime"
              sub="Used on non-KVM hosts (e.g. nested VMs or --bench bare-metal runs) with R2-backed cells mounted via cladefsd."
              right={
                tenantId !== undefined && (
                  <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
                    managed tenant_id = {String(tenantId)}
                  </Badge>
                )
              }
            >
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <Field id="fuse-enabled" label="Host-FUSE enabled" badge={<RestartTag />}>
                  <TriSelect
                    id="fuse-enabled"
                    value={draft.cladefsFuse.enabled}
                    disabled={!isOrgAdmin}
                    inheritLabel={inheritedTriLabel(
                      !isOrgDefaults ? orgDefaultsCfg?.cladefsFuse?.enabled : undefined,
                      false,
                    )}
                    onChange={(v) => setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, enabled: v } }))}
                  />
                </Field>

                <Field id="fuse-default-rt" label="Use FUSE as default runtime">
                  <TriSelect
                    id="fuse-default-rt"
                    value={draft.cladefsFuse.defaultRuntime}
                    disabled={!isOrgAdmin}
                    inheritLabel={inheritedTriLabel(
                      !isOrgDefaults ? orgDefaultsCfg?.cladefsFuse?.defaultRuntime : undefined,
                      false,
                    )}
                    onChange={(v) => setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, defaultRuntime: v } }))}
                  />
                </Field>

                <Field
                  id="fuse-disk"
                  label="Cell disk size (bytes)"
                  hint={formatBytesHint(draft.cladefsFuse.diskBytes)}
                >
                  <Input
                    id="fuse-disk"
                    value={draft.cladefsFuse.diskBytes}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.diskBytes || "bytes"}
                    onChange={(e) => setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, diskBytes: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="fuse-peer" label="CladeFS peer address">
                  <Input
                    id="fuse-peer"
                    value={draft.cladefsFuse.cladefsPeerAddr}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.cladefsPeerAddr || "100.x.y.z:9185"}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, cladefsPeerAddr: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="fuse-ctrl-sock" label="CladeFS control socket" badge={<RestartTag />}>
                  <Input
                    id="fuse-ctrl-sock"
                    value={draft.cladefsFuse.cladefsControlSock}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.cladefsControlSock || "/run/cladefsd/control.sock"}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, cladefsControlSock: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="fuse-cell-root" label="Cell root" badge={<RestartTag />}>
                  <Input
                    id="fuse-cell-root"
                    value={draft.cladefsFuse.cellRoot}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.cellRoot || "/var/lib/cladefs/cells"}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, cellRoot: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="fuse-mount-root" label="Mount root" badge={<RestartTag />}>
                  <Input
                    id="fuse-mount-root"
                    value={draft.cladefsFuse.mountRoot}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.mountRoot || "/var/lib/hotbed/worker/fuse-mnt"}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, mountRoot: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>

                <Field id="fuse-mount-stores" label="Mount stores root" badge={<RestartTag />}>
                  <Input
                    id="fuse-mount-stores"
                    value={draft.cladefsFuse.mountStoresRoot}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.cladefsFuse.mountStoresRoot || "/var/lib/hotbed/worker/fuse-stores"}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, cladefsFuse: { ...d.cladefsFuse, mountStoresRoot: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>
          </TabsContent>

          {/* 5. SYSTEM BINARIES & UPGRADES */}
          <TabsContent value="system" className="space-y-5">
            <Card
              title="Host Paths & Service"
              sub="Applied only when you explicitly trigger a system upgrade (restarting cladefsd terminates live VMs and FUSE mounts)."
            >
              <div className="grid gap-4 sm:grid-cols-3">
                <Field id="sys-cladefsd-bin" label="cladefsd install path">
                  <Input
                    id="sys-cladefsd-bin"
                    value={draft.system.cladefsdBin}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.system.cladefsdBin || BUILTIN_DEFAULTS.system.cladefsdBin}
                    onChange={(e) => setDraft((d) => ({ ...d, system: { ...d.system, cladefsdBin: e.target.value } }))}
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
                <Field id="sys-cladefsd-svc" label="cladefsd systemd service">
                  <Input
                    id="sys-cladefsd-svc"
                    value={draft.system.cladefsdService}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.system.cladefsdService || BUILTIN_DEFAULTS.system.cladefsdService}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, system: { ...d.system, cladefsdService: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
                <Field id="sys-fc-bin" label="firecracker install path">
                  <Input
                    id="sys-fc-bin"
                    value={draft.system.firecrackerBin}
                    disabled={!isOrgAdmin}
                    placeholder={orgDefaultsDraft.system.firecrackerBin || BUILTIN_DEFAULTS.system.firecrackerBin}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, system: { ...d.system, firecrackerBin: e.target.value } }))
                    }
                    className="bg-background font-mono text-[12px]"
                  />
                </Field>
              </div>
            </Card>

            <div className="grid gap-5 xl:grid-cols-2">
              <ArtifactEditor
                title="Managed cladefsd Binaries"
                sub="Staged and SHA-256 verified before restarting cladefsd during system upgrade."
                map={draft.system.cladefsd}
                inheritedMap={!isOrgDefaults ? orgDefaultsDraft.system.cladefsd : undefined}
                disabled={!isOrgAdmin}
                onChange={(arch, patch) =>
                  setDraft((d) => ({
                    ...d,
                    system: {
                      ...d.system,
                      cladefsd: { ...d.system.cladefsd, [arch]: { ...d.system.cladefsd[arch], ...patch } },
                    },
                  }))
                }
              />
              <ArtifactEditor
                title="Managed Firecracker Binaries"
                sub="Installed atomically to firecracker_bin during system upgrade."
                map={draft.system.firecracker}
                inheritedMap={!isOrgDefaults ? orgDefaultsDraft.system.firecracker : undefined}
                disabled={!isOrgAdmin}
                onChange={(arch, patch) =>
                  setDraft((d) => ({
                    ...d,
                    system: {
                      ...d.system,
                      firecracker: { ...d.system.firecracker, [arch]: { ...d.system.firecracker[arch], ...patch } },
                    },
                  }))
                }
              />
            </div>

            <Card
              title="Run Host System Upgrade"
              sub={`Applies staged cladefsd, firecracker, and guest kernel updates for ${orgLabel} nodes one at a time. Drains active commands and releases workspaces on each target node.`}
            >
              <div className="flex flex-wrap items-center gap-2.5">
                {isOrgAdmin ? (
                  <>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      disabled={upgradeSystem.isPending || isDirty}
                      onClick={() => void onTriggerUpgrade(scope)}
                      className="gap-1.5 font-mono text-[11.5px]"
                    >
                      <Zap className="size-3.5 text-primary" />
                      {upgradeSystem.isPending
                        ? "Upgrading…"
                        : isOrgDefaults
                          ? "Rolling upgrade all online org nodes"
                          : `Upgrade ${scope}`}
                    </Button>
                    {isDirty && (
                      <span className="font-mono text-[11px] text-muted-foreground">
                        Save pending artifact changes before triggering an upgrade.
                      </span>
                    )}
                  </>
                ) : (
                  <span className="font-mono text-[11.5px] text-muted-foreground">
                    Only organization admins can trigger host system upgrades.
                  </span>
                )}
              </div>

              {upgradeSystem.error && (
                <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
                  <AlertDescription className="text-[11px] text-critical">
                    {String(upgradeSystem.error)}
                  </AlertDescription>
                </Alert>
              )}

              {upgradeResults && upgradeResults.length > 0 && (
                <div className="mt-4 space-y-1.5">
                  {upgradeResults.map((r) => (
                    <div
                      key={r.node}
                      className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border/80 bg-background/40 px-3 py-2 font-mono text-[11.5px]"
                    >
                      <div className="flex items-center gap-2">
                        <Server className="size-3.5 text-muted-foreground" />
                        <span className="font-medium text-foreground">{r.node}</span>
                        <Badge
                          variant="secondary"
                          className={
                            r.error
                              ? "bg-critical/15 text-critical"
                              : r.status === "upgraded"
                                ? "bg-ember-wash text-primary"
                                : "text-muted-foreground"
                          }
                        >
                          {r.status}
                        </Badge>
                      </div>
                      <div className="flex flex-wrap items-center gap-2 text-[10.5px] text-muted-foreground">
                        {r.cladefsdVersion && <span>cladefsd {r.cladefsdVersion}</span>}
                        {r.firecrackerVersion && <span>fc {r.firecrackerVersion}</span>}
                        {r.kernelVersion && <span>kernel {r.kernelVersion}</span>}
                        <span>
                          stopped {r.stoppedCommands} · released {r.releasedWorkspaces}
                        </span>
                        {r.error && <span className="text-critical">{r.error}</span>}
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </Card>
          </TabsContent>

          {/* 6. GIT CREDENTIALS (ORG-SCOPED) */}
          <TabsContent value="git" className="space-y-5">
            <Card
              title="Organization Git Credentials"
              sub={`Stored write-only inside ${orgLabel}'s isolated Controller DO and delegated as short-lived tokens to this organization's workers, bases, and commands.`}
              right={
                <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
                  <GitBranch className="mr-1 size-3" /> org-scoped
                </Badge>
              }
            >
              {gitCredsQuery.isLoading ? (
                <div className="py-6 text-center font-mono text-[11.5px] text-muted-foreground">
                  loading git credentials…
                </div>
              ) : gitCredsQuery.error ? (
                <Alert variant="destructive" className="border-critical/40 font-mono text-[11px]">
                  <AlertDescription className="text-[11px] text-critical">
                    {String(gitCredsQuery.error)}
                  </AlertDescription>
                </Alert>
              ) : gitCredsQuery.data && gitCredsQuery.data.length > 0 ? (
                <div className="space-y-2">
                  {gitCredsQuery.data.map((c) => (
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
                          {c.updatedAt > 0n ? ` · updated ${ago(c.updatedAt, nowMs)}` : ""}
                        </div>
                      </div>
                      {isOrgAdmin && (
                        <Button
                          type="button"
                          variant="outline"
                          size="xs"
                          disabled={deleteGitCred.isPending}
                          onClick={() => deleteGitCred.mutate(c.name)}
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
              {deleteGitCred.error && (
                <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
                  <AlertDescription className="text-[11px] text-critical">
                    {String(deleteGitCred.error)}
                  </AlertDescription>
                </Alert>
              )}
            </Card>

            {isOrgAdmin && (
              <div className="grid gap-5 lg:grid-cols-2">
                <Card
                  title="Sign in with GitHub (OAuth Device Flow)"
                  sub="Recommended: mints a refreshable GitHub OAuth credential directly on this organization's controller."
                >
                  <form onSubmit={onStartGitHub} className="space-y-3">
                    <div className="grid gap-3 sm:grid-cols-2">
                      <Field id="gh-name" label="Credential name">
                        <Input
                          id="gh-name"
                          value={ghLoginName}
                          onChange={(e) => setGhLoginName(e.target.value)}
                          placeholder="github"
                          className="bg-background font-mono text-[12px]"
                          required
                        />
                      </Field>
                      <Field id="gh-host" label="Git host">
                        <Input
                          id="gh-host"
                          value={ghLoginHost}
                          onChange={(e) => setGhLoginHost(e.target.value)}
                          placeholder="github.com"
                          className="bg-background font-mono text-[12px]"
                        />
                      </Field>
                    </div>
                    <Field
                      id="gh-repos"
                      label="Repo patterns (optional)"
                      hint="Comma-separated owner/* or owner/repo; leave blank for all repos on host."
                    >
                      <Input
                        id="gh-repos"
                        value={ghLoginRepos}
                        onChange={(e) => setGhLoginRepos(e.target.value)}
                        placeholder="my-org/*, my-org/my-repo"
                        className="bg-background font-mono text-[12px]"
                      />
                    </Field>
                    <Button type="submit" size="sm" disabled={startGhLogin.isPending} className="gap-1.5 font-mono text-[11.5px]">
                      <KeyRound className="size-3.5" />
                      {startGhLogin.isPending ? "Starting…" : "Start GitHub login"}
                    </Button>
                  </form>

                  {(startGhLogin.error || completeGhLogin.error) && (
                    <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
                      <AlertDescription className="text-[11px] text-critical">
                        {String(startGhLogin.error ?? completeGhLogin.error)}
                      </AlertDescription>
                    </Alert>
                  )}

                  {ghChallenge && (
                    <Alert className="mt-4 rounded-xl border-primary/50 bg-ember-wash/60 p-3.5">
                      <AlertTitle className="font-mono text-[11.5px] font-medium text-primary">
                        Approve on GitHub, then click Complete
                      </AlertTitle>
                      <AlertDescription className="mt-2 space-y-2.5 text-[12px]">
                        <div className="flex flex-wrap items-center gap-2">
                          <code className="rounded-lg border border-primary/40 bg-background px-3 py-1 font-mono text-[15px] font-semibold tracking-wider text-foreground">
                            {ghChallenge.userCode}
                          </code>
                          <Button
                            type="button"
                            variant="outline"
                            size="xs"
                            onClick={() => void copyText("gh-code", ghChallenge.userCode)}
                            className="font-mono text-[10.5px]"
                          >
                            <Copy className="mr-1 size-3" />
                            {copiedKey === "gh-code" ? "Copied" : "Copy code"}
                          </Button>
                          <a
                            href={ghChallenge.verificationUri}
                            target="_blank"
                            rel="noreferrer"
                            className="inline-flex items-center gap-1 font-mono text-[11px] text-primary underline underline-offset-4"
                          >
                            {ghChallenge.verificationUri}
                            <ExternalLink className="size-3" />
                          </a>
                        </div>
                        <div className="flex items-center gap-2">
                          <Button
                            type="button"
                            size="sm"
                            disabled={completeGhLogin.isPending}
                            onClick={() => void onCompleteGitHub()}
                            className="font-mono text-[11.5px]"
                          >
                            {completeGhLogin.isPending ? "Waiting for GitHub…" : "Complete login"}
                          </Button>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => setGhChallenge(null)}
                            className="font-mono text-[11px] text-muted-foreground"
                          >
                            Cancel
                          </Button>
                        </div>
                      </AlertDescription>
                    </Alert>
                  )}
                </Card>

                <Card
                  title="Add Static Git Token"
                  sub="Store a personal access token or fine-grained token write-only for headless CI or non-GitHub hosts."
                >
                  <form onSubmit={onSavePat} className="space-y-3">
                    <div className="grid gap-3 sm:grid-cols-2">
                      <Field id="pat-name" label="Credential name">
                        <Input
                          id="pat-name"
                          value={patName}
                          onChange={(e) => setPatName(e.target.value)}
                          placeholder="github-ci"
                          className="bg-background font-mono text-[12px]"
                          required
                        />
                      </Field>
                      <Field id="pat-host" label="Git host">
                        <Input
                          id="pat-host"
                          value={patHost}
                          onChange={(e) => setPatHost(e.target.value)}
                          placeholder="github.com"
                          className="bg-background font-mono text-[12px]"
                        />
                      </Field>
                      <Field id="pat-user" label="Username (optional)">
                        <Input
                          id="pat-user"
                          value={patUser}
                          onChange={(e) => setPatUser(e.target.value)}
                          placeholder="x-access-token"
                          className="bg-background font-mono text-[12px]"
                        />
                      </Field>
                      <Field id="pat-repos" label="Repo patterns (optional)">
                        <Input
                          id="pat-repos"
                          value={patRepos}
                          onChange={(e) => setPatRepos(e.target.value)}
                          placeholder="owner/* (blank = all)"
                          className="bg-background font-mono text-[12px]"
                        />
                      </Field>
                    </div>
                    <Field id="pat-api" label="API base URL (optional, for GHE)">
                      <Input
                        id="pat-api"
                        value={patApiUrl}
                        onChange={(e) => setPatApiUrl(e.target.value)}
                        placeholder="https://github.company.com/api/v3"
                        className="bg-background font-mono text-[12px]"
                      />
                    </Field>
                    <Field id="pat-token" label="Token (stored write-only)">
                      <Input
                        id="pat-token"
                        type="password"
                        value={patToken}
                        onChange={(e) => setPatToken(e.target.value)}
                        placeholder="ghp_… or github_pat_…"
                        className="bg-background font-mono text-[12px]"
                        required
                      />
                    </Field>
                    <Button
                      type="submit"
                      size="sm"
                      disabled={putGitCred.isPending || !patName.trim() || !patToken.trim()}
                      className="font-mono text-[11.5px]"
                    >
                      {putGitCred.isPending ? "Saving…" : "Save credential"}
                    </Button>
                  </form>
                  {putGitCred.error && (
                    <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
                      <AlertDescription className="text-[11px] text-critical">
                        {String(putGitCred.error)}
                      </AlertDescription>
                    </Alert>
                  )}
                </Card>
              </div>
            )}
          </TabsContent>

          {/* 7. RAW JSON & CLI */}
          <TabsContent value="json" className="space-y-5">
            <div className="grid gap-5 lg:grid-cols-2">
              <Card
                title={isOrgDefaults ? "Organization Defaults JSON" : `Node Override JSON (${scope})`}
                sub="Edit exact WorkerConfig JSON for this scope. Saving replaces this scope atomically with optimistic revision checking."
                right={
                  <div className="flex items-center gap-1.5">
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      onClick={() => {
                        try {
                          const input = draftToWorkerConfigInput(draft, { isOrgDefaults });
                          setRawJsonText(workerConfigToPrettyJson(input));
                          setRawJsonErr(null);
                        } catch (err) {
                          setRawJsonErr(err instanceof Error ? err.message : String(err));
                        }
                      }}
                      className="font-mono text-[10.5px]"
                    >
                      Sync from form
                    </Button>
                    {isOrgAdmin && (
                      <Button
                        type="button"
                        size="xs"
                        disabled={setConfig.isPending}
                        onClick={() => void onSaveRawJson()}
                        className="font-mono text-[10.5px]"
                      >
                        Save raw JSON
                      </Button>
                    )}
                  </div>
                }
              >
                <Textarea
                  aria-label="Raw WorkerConfig JSON"
                  rows={18}
                  value={rawJsonText}
                  disabled={!isOrgAdmin}
                  onChange={(e) => setRawJsonText(e.target.value)}
                  className="bg-background font-mono text-[11.5px]"
                />
                {rawJsonErr && (
                  <Alert variant="destructive" className="mt-3 border-critical/40 font-mono text-[11px]">
                    <AlertDescription className="text-[11px] text-critical">{rawJsonErr}</AlertDescription>
                  </Alert>
                )}
              </Card>

              <div className="space-y-5">
                {!isOrgDefaults && (
                  <Card
                    title={`Effective Config (${scope})`}
                    sub="Server-merged result of Organization defaults + node override (read-only)."
                  >
                    <pre className="max-h-72 overflow-auto rounded-xl border border-border bg-background/60 p-3 font-mono text-[11px] text-foreground">
                      {workerConfigToPrettyJson(effectiveCfg)}
                    </pre>
                  </Card>
                )}

                <Card
                  title="Equivalent CLI Command"
                  sub="Run this in your terminal to apply the current form state to the active organization."
                  right={
                    <Button
                      type="button"
                      variant="outline"
                      size="xs"
                      onClick={() => void copyText("cli", cliPreview)}
                      className="font-mono text-[10.5px]"
                    >
                      <Copy className="mr-1 size-3" />
                      {copiedKey === "cli" ? "Copied" : "Copy"}
                    </Button>
                  }
                >
                  <pre className="max-h-72 overflow-auto rounded-xl border border-border bg-background/60 p-3 font-mono text-[11px] text-foreground">
                    {cliPreview}
                  </pre>
                </Card>
              </div>
            </div>
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}
