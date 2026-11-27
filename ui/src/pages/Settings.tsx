import { useNavigate, useSearch } from "@tanstack/react-router";
import { Check, Copy, RefreshCw, RotateCcw, Save, ShieldAlert, SlidersHorizontal, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import type { WorkerConfig, WorkerConfigInput } from "../api";
import { ArtifactEditor, ConfigFields, ConfigSection, type ConfigForm } from "../components/ConfigSection";
import { CopyButton } from "../components/CopyButton";
import { GitAuthSettings } from "../components/GitAuthSettings";
import { Card } from "../components/Panel";
import { SystemUpgradePanel } from "../components/SystemUpgradePanel";
import { isOrgAdmin, useAuthMe } from "../lib/auth";
import { useSnapshot } from "../lib/data";
import { ago } from "../lib/format";
import {
  BUILTIN_DEFAULTS,
  draftRequiresRestart,
  draftSyncAction,
  draftToCliCommand,
  draftToWorkerConfigInput,
  parseWorkerConfigJson,
  secondsLabel,
  useDeleteWorkerConfig,
  useSetWorkerConfig,
  useWorkerConfig,
  workerConfigToDraft,
  workerConfigToPrettyJson,
  type SettingsDraft,
} from "../lib/settings";
import {
  CONTROLLER_SECTION,
  FRESHEN_PRESETS,
  FRESHEN_SECTION,
  FUSE_SECTION,
  RUNTIME_SECTION,
  SYSTEM_ARTIFACTS,
  SYSTEM_SECTION,
  VM_ARTIFACTS,
  VM_COMPUTE_SECTION,
  VM_HOST_SECTION,
} from "../lib/settingsFields";
import { PageTitle } from "./Nodes";

export type SettingsTab = "freshen" | "runtime" | "vm" | "fuse" | "system" | "git" | "json";

export interface SettingsSearch {
  scope?: string;
  tab?: SettingsTab;
}

const TABS: readonly { value: SettingsTab; label: string }[] = [
  { value: "freshen", label: "Base Freshening" },
  { value: "runtime", label: "Runtime" },
  { value: "vm", label: "Firecracker VM" },
  { value: "fuse", label: "CladeFS FUSE" },
  { value: "system", label: "System & Upgrades" },
  { value: "git", label: "Git Auth" },
  { value: "json", label: "Raw JSON & CLI" },
];

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function SettingsPage() {
  const search = useSearch({ from: "/settings" });
  const navigate = useNavigate();
  const { data: auth } = useAuthMe();
  const { data: snap } = useSnapshot();

  const scope = (search.scope ?? "").trim();
  const isOrgDefaults = scope === "";
  const activeTab: SettingsTab = TABS.some((t) => t.value === search.tab) ? (search.tab as SettingsTab) : "freshen";
  // Git credentials belong to the organization, so their tab has no node scope or config save bar.
  const configTab = activeTab !== "git";

  const activeOrg = auth?.organization ?? null;
  const canEdit = isOrgAdmin(auth);

  // Fetch all org scopes (defaults + list of node overrides) and, when inspecting
  // a node override, fetch that node's server-computed effective config too.
  const allCfgQuery = useWorkerConfig("");
  const nodeCfgQuery = useWorkerConfig(scope);
  // keepPreviousData would otherwise show the last node's config under this scope.
  const nodeCfgPlaceholderless = nodeCfgQuery.isPlaceholderData ? undefined : nodeCfgQuery.data;

  const setConfig = useSetWorkerConfig();
  const deleteConfig = useDeleteWorkerConfig();

  const orgDefaultsCfg: WorkerConfig | undefined = allCfgQuery.data?.defaults;
  const savedNodeOverrides: WorkerConfig[] = allCfgQuery.data?.nodes ?? [];
  const activeScopeCfg: WorkerConfig | undefined = isOrgDefaults
    ? orgDefaultsCfg
    : savedNodeOverrides.find((n) => n.node === scope) ?? nodeCfgPlaceholderless?.nodes?.[0];
  const effectiveCfg: WorkerConfig | undefined = isOrgDefaults ? orgDefaultsCfg : nodeCfgPlaceholderless?.effective;

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
  const [confirmReset, setConfirmReset] = useState(false);
  const [customNodeInput, setCustomNodeInput] = useState("");

  const isDirty = useMemo(() => JSON.stringify(draft) !== JSON.stringify(baselineDraft), [draft, baselineDraft]);

  // The draft follows the saved config only while it holds no unsaved edits, or
  // when the scope changes: a background refetch must not wipe what is typed.
  const baselineJson = JSON.stringify(baselineDraft);
  const [synced, setSynced] = useState({ scope, sig: baselineSig, draftJson: baselineJson });
  const draftJson = JSON.stringify(draft);
  const hasUnsavedEdits = draftJson !== synced.draftJson && draftJson !== baselineJson;
  const syncAction = draftSyncAction(synced, { scope, sig: baselineSig }, hasUnsavedEdits);
  const remoteChangedWhileDirty = syncAction === "hold";

  function resetDraftToBaseline() {
    setDraft(baselineDraft);
    setRawJsonText(workerConfigToPrettyJson(activeScopeCfg));
    setRawJsonErr(null);
    setSaveErr(null);
    setSynced({ scope, sig: baselineSig, draftJson: baselineJson });
  }

  useEffect(() => {
    if (syncAction === "reset") resetDraftToBaseline();
  }, [syncAction]);

  const needsRestart = useMemo(() => draftRequiresRestart(baselineDraft, draft), [baselineDraft, draft]);

  const form: ConfigForm = {
    draft,
    inherited: isOrgDefaults ? undefined : orgDefaultsDraft,
    disabled: !canEdit,
    update: setDraft,
  };

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

  function go(next: { scope: string; tab: SettingsTab }) {
    void navigate({
      to: "/settings",
      search: { scope: next.scope || undefined, tab: next.tab === "freshen" ? undefined : next.tab },
    });
  }

  function setScope(nextScope: string) {
    setSaveOk(null);
    setSaveErr(null);
    go({ scope: nextScope, tab: activeTab });
  }

  /** Save a whole config for this scope, replacing what is there if nobody changed it since it loaded. */
  async function save(config: WorkerConfigInput, ok: (rev: bigint) => string) {
    const saved = await setConfig.mutateAsync({
      node: scope,
      config,
      replace: true,
      ifRevision: activeScopeCfg?.revision ?? 0n,
    });
    setDraft(workerConfigToDraft(saved));
    setRawJsonText(workerConfigToPrettyJson(saved));
    setSaveOk(ok(saved.revision));
  }

  async function onSave() {
    if (!canEdit) return;
    setSaveErr(null);
    setSaveOk(null);
    try {
      await save(draftToWorkerConfigInput(draft, { isOrgDefaults }), (rev) =>
        isOrgDefaults ? `Saved organization defaults (rev ${rev}).` : `Saved override for ${scope} (rev ${rev}).`,
      );
    } catch (err) {
      setSaveErr(errorText(err));
    }
  }

  async function onSaveRawJson() {
    if (!canEdit) return;
    setRawJsonErr(null);
    setSaveErr(null);
    setSaveOk(null);
    try {
      await save(parseWorkerConfigJson(rawJsonText), (rev) => `Saved raw JSON (rev ${rev}).`);
    } catch (err) {
      setRawJsonErr(errorText(err));
    }
  }

  async function onResetScope() {
    if (!canEdit) return;
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
      setSaveErr(errorText(err));
    }
  }

  const cliPreview = useMemo(() => {
    try {
      return draftToCliCommand(scope, draftToWorkerConfigInput(draft, { isOrgDefaults }));
    } catch {
      return `# Fix validation errors in the form to preview the CLI command`;
    }
  }, [draft, isOrgDefaults, scope]);

  const nowMs = Number(snap?.now ?? Date.now());
  const orgLabel = activeOrg?.name ?? "Organization";
  const loadingCfg = allCfgQuery.isLoading || (!isOrgDefaults && nodeCfgQuery.isLoading);
  const cfgLoadErr = allCfgQuery.error ?? (!isOrgDefaults ? nodeCfgQuery.error : null);

  const tenantBadge = tenantId !== undefined && (
    <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
      managed tenant_id = {String(tenantId)}
    </Badge>
  );

  return (
    <div className="space-y-6">
      <PageTitle
        title={activeOrg ? `${activeOrg.name} Settings` : "Organization Settings"}
        sub={
          activeOrg
            ? `Scoped exclusively to ${activeOrg.name} (${activeOrg.id}) · isolated Cloudflare Durable Object · cluster ${snap?.clusterId ?? "…"} · never shared across organizations`
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
                canEdit ? "bg-ember-wash font-mono text-[10.5px] text-primary" : "bg-muted font-mono text-[10.5px] text-muted-foreground"
              }
            >
              {canEdit ? (activeOrg?.role ? `${activeOrg.role} · editable` : "editable") : `${activeOrg?.role ?? "member"} · read-only`}
            </Badge>
          </div>
        }
      />

      {!canEdit && (
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
      {configTab && (
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

            {canEdit && (
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
      )}

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

      {remoteChangedWhileDirty && (
        <Alert className="rounded-xl border-warning/40 font-mono text-[11.5px]">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <AlertDescription className="text-[11.5px] text-warning-ink">
              This config changed elsewhere. Your unsaved edits are kept; saving may fail on the revision check.
            </AlertDescription>
            <Button type="button" variant="outline" size="xs" onClick={resetDraftToBaseline} className="font-mono text-[10.5px]">
              <RefreshCw className="mr-1 size-3" /> Discard edits and reload
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
      {configTab && (
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
              <span className={`font-mono text-[10.5px] ${needsRestart ? "text-warning-ink" : "text-good-ink"}`}>
                {needsRestart
                  ? "· structural change (idle-deferred worker restart)"
                  : "· hot-reloads live on next heartbeat (no restart)"}
              </span>
            )}
          </div>

          {canEdit && (
            <div className="flex flex-wrap items-center gap-2">
              {activeScopeCfg && activeScopeCfg.revision > 0n && (
                <AlertDialog open={confirmReset} onOpenChange={setConfirmReset}>
                  <AlertDialogTrigger
                    render={
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        disabled={deleteConfig.isPending || setConfig.isPending}
                        className="gap-1.5 font-mono text-[11px] text-muted-foreground hover:border-critical/60 hover:text-critical"
                      />
                    }
                  >
                    <Trash2 className="size-3.5" />
                    {isOrgDefaults ? "Reset org defaults" : `Remove ${scope} override`}
                  </AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>
                        {isOrgDefaults ? "Reset organization defaults?" : `Remove the override for ${scope}?`}
                      </AlertDialogTitle>
                      <AlertDialogDescription>
                        {isOrgDefaults
                          ? "This deletes the saved organization defaults. Every node without its own override falls back to built-in defaults."
                          : `This deletes the saved override for ${scope}. The node inherits the organization defaults again.`}
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>Cancel</AlertDialogCancel>
                      <AlertDialogAction
                        variant="destructive"
                        onClick={() => {
                          setConfirmReset(false);
                          void onResetScope();
                        }}
                      >
                        {isOrgDefaults ? "Reset org defaults" : "Remove override"}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              )}
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={!isDirty || setConfig.isPending}
                onClick={resetDraftToBaseline}
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
        <Tabs value={activeTab} onValueChange={(v) => v && go({ scope, tab: v as SettingsTab })} className="space-y-5">
          <TabsList className="h-auto w-full flex-wrap justify-start gap-1 rounded-xl border border-border bg-card/70 p-1">
            {TABS.map((t) => (
              <TabsTrigger key={t.value} value={t.value} className="px-3 py-1.5 font-mono text-[11.5px]">
                {t.label}
              </TabsTrigger>
            ))}
          </TabsList>

          <TabsContent value="freshen" className="space-y-5">
            <ConfigSection
              section={FRESHEN_SECTION}
              form={form}
              right={
                canEdit && (
                  <div className="flex flex-wrap gap-1.5">
                    {FRESHEN_PRESETS.map((p) => (
                      <Button
                        key={p.label}
                        type="button"
                        variant="outline"
                        size="xs"
                        onClick={() => setDraft((d) => ({ ...d, freshen: { ...d.freshen, ...p.freshen } }))}
                        className="font-mono text-[10.5px]"
                      >
                        {p.label}
                      </Button>
                    ))}
                  </div>
                )
              }
            />

            <Card
              title={CONTROLLER_SECTION.title}
              sub={CONTROLLER_SECTION.sub}
              right={
                !isOrgDefaults && (
                  <Button type="button" variant="outline" size="xs" onClick={() => setScope("")} className="font-mono text-[10.5px]">
                    Edit in Org defaults →
                  </Button>
                )
              }
            >
              {isOrgDefaults ? (
                <ConfigFields section={CONTROLLER_SECTION} form={form} />
              ) : (
                <p className="font-mono text-[11.5px] text-muted-foreground">
                  Applies to the organization as a whole (currently{" "}
                  <span className="text-foreground">
                    {secondsLabel(
                      orgDefaultsCfg?.controller?.sessionIdleTimeoutSeconds ?? BUILTIN_DEFAULTS.controller.sessionIdleTimeoutSeconds,
                    )}
                  </span>{" "}
                  session idle timeout ·{" "}
                  <span className="text-foreground">
                    {secondsLabel(orgDefaultsCfg?.controller?.offlineGraceSeconds ?? BUILTIN_DEFAULTS.controller.offlineGraceSeconds)}
                  </span>{" "}
                  offline grace). Switch scope to Org defaults to change.
                </p>
              )}
            </Card>
          </TabsContent>

          <TabsContent value="runtime" className="space-y-5">
            <ConfigSection section={RUNTIME_SECTION} form={form} />
          </TabsContent>

          <TabsContent value="vm" className="space-y-5">
            <ConfigSection section={VM_COMPUTE_SECTION} form={form} right={tenantBadge} />
            <ConfigSection section={VM_HOST_SECTION} form={form} />
            <div className="grid gap-5 xl:grid-cols-2">
              {VM_ARTIFACTS.map((a) => (
                <ArtifactEditor key={a.title} spec={a} form={form} />
              ))}
            </div>
          </TabsContent>

          <TabsContent value="fuse" className="space-y-5">
            <ConfigSection section={FUSE_SECTION} form={form} right={tenantBadge} />
          </TabsContent>

          <TabsContent value="system" className="space-y-5">
            <ConfigSection section={SYSTEM_SECTION} form={form} />
            <div className="grid gap-5 xl:grid-cols-2">
              {SYSTEM_ARTIFACTS.map((a) => (
                <ArtifactEditor key={a.title} spec={a} form={form} />
              ))}
            </div>
            <SystemUpgradePanel node={scope} orgLabel={orgLabel} canEdit={canEdit} pendingEdits={isDirty} />
          </TabsContent>

          <TabsContent value="git" className="space-y-5">
            <GitAuthSettings orgLabel={orgLabel} canEdit={canEdit} now={nowMs} />
          </TabsContent>

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
                          setRawJsonText(workerConfigToPrettyJson(draftToWorkerConfigInput(draft, { isOrgDefaults })));
                          setRawJsonErr(null);
                        } catch (err) {
                          setRawJsonErr(errorText(err));
                        }
                      }}
                      className="font-mono text-[10.5px]"
                    >
                      Sync from form
                    </Button>
                    {canEdit && (
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
                  disabled={!canEdit}
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
                  right={<CopyButton text={cliPreview} icon={<Copy className="size-3" />} />}
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
