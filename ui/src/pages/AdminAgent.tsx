import {
  ArrowLeft,
  Database,
  DatabaseZap,
  Loader2,
  Play,
  Table2,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { DataTableView, SqlBlockView } from "../components/AdminSqlViews";
import { PageTitle } from "../components/PageTitle";
import {
  useAdminAgentStatus,
  useInspectDoSchema,
  useRunDoSql,
  type AdminAgentStatus,
  type DoBindingName,
} from "../lib/adminAgent";
import { DoLinkContext, formatDoTargetLabel, type DoTargetRef } from "../lib/doLinks";
import { bytes } from "../lib/format";

const DEFAULT_TABLES_SQL =
  "SELECT name, type FROM sqlite_master WHERE type = 'table' ORDER BY name";

interface SqlHistoryEntry {
  binding: DoBindingName;
  addressMode: "name" | "id";
  name: string;
  id: string;
  sql: string;
}

function SqlExplorerPanel({
  statusInfo,
  pendingTarget,
  onConsumePendingTarget,
}: {
  statusInfo: AdminAgentStatus | undefined;
  pendingTarget: { target: DoTargetRef; seq: number } | null;
  onConsumePendingTarget: () => void;
}) {
  const defaultCtrlName = statusInfo?.scope.controllerDoName ?? "default";
  const allowedBindings = statusInfo?.bindings ?? (["CONTROLLER", "COMMAND", "WORKER"] as DoBindingName[]);
  const bindingItems = useMemo(() => {
    const labels: Record<DoBindingName, string> = {
      CONTROLLER: "CONTROLLER (ControllerDO)",
      COMMAND: "COMMAND (CommandDO)",
      WORKER: "WORKER (WorkerDO)",
    };
    return allowedBindings.map((b) => ({ value: b, label: labels[b] }));
  }, [allowedBindings]);

  const [binding, setBinding] = useState<DoBindingName>("CONTROLLER");
  const [addressMode, setAddressMode] = useState<"name" | "id">("name");
  const [targetName, setTargetName] = useState(defaultCtrlName);
  const [targetId, setTargetId] = useState("");
  const [sql, setSql] = useState(DEFAULT_TABLES_SQL);
  const [navHistory, setNavHistory] = useState<SqlHistoryEntry[]>([]);
  const lastAppliedSeqRef = useRef(0);
  const prevDefaultCtrlRef = useRef(defaultCtrlName);

  const runSql = useRunDoSql();
  const runSqlMutateRef = useRef(runSql.mutate);
  runSqlMutateRef.current = runSql.mutate;

  useEffect(() => {
    if (prevDefaultCtrlRef.current === defaultCtrlName) return;
    prevDefaultCtrlRef.current = defaultCtrlName;
    setNavHistory([]);
    if (binding === "CONTROLLER") {
      setTargetName(defaultCtrlName);
    }
  }, [defaultCtrlName, binding]);

  useEffect(() => {
    if (!pendingTarget || pendingTarget.seq === lastAppliedSeqRef.current) return;
    lastAppliedSeqRef.current = pendingTarget.seq;
    const next = pendingTarget.target;
    const nextMode: "name" | "id" = next.id ? "id" : "name";
    const nextName = next.id ? "" : (next.name ?? defaultCtrlName);
    const nextId = next.id ?? "";

    if (
      binding !== next.binding ||
      addressMode !== nextMode ||
      (nextMode === "name" ? targetName !== nextName : targetId !== nextId)
    ) {
      setNavHistory((prev) => [
        ...prev.slice(-14),
        { binding, addressMode, name: targetName, id: targetId, sql },
      ]);
    }

    setBinding(next.binding);
    setAddressMode(nextMode);
    setTargetName(nextName);
    setTargetId(nextId);
    setSql(DEFAULT_TABLES_SQL);
    runSqlMutateRef.current({
      binding: next.binding,
      name: nextMode === "name" ? nextName.trim() || undefined : undefined,
      id: nextMode === "id" ? nextId.trim() || undefined : undefined,
      sql: DEFAULT_TABLES_SQL,
      maxRows: 200,
    });
    onConsumePendingTarget();
  }, [pendingTarget, binding, addressMode, targetName, targetId, sql, defaultCtrlName, onConsumePendingTarget]);

  const schemaQuery = useInspectDoSchema(
    {
      binding,
      name: addressMode === "name" ? targetName : undefined,
      id: addressMode === "id" ? targetId : undefined,
    },
    Boolean(addressMode === "name" ? targetName.trim() || binding === "CONTROLLER" : targetId.trim().length === 64),
  );

  function executeQuery(overrideSql?: string) {
    const query = (overrideSql ?? sql).trim();
    if (!query) return;
    runSql.mutate({
      binding,
      name: addressMode === "name" ? targetName.trim() || undefined : undefined,
      id: addressMode === "id" ? targetId.trim() || undefined : undefined,
      sql: query,
      maxRows: 200,
    });
  }

  function handleBack() {
    const prev = navHistory[navHistory.length - 1];
    if (!prev) return;
    setNavHistory((h) => h.slice(0, -1));
    setBinding(prev.binding);
    setAddressMode(prev.addressMode);
    setTargetName(prev.name);
    setTargetId(prev.id);
    setSql(prev.sql);
    runSql.mutate({
      binding: prev.binding,
      name: prev.addressMode === "name" ? prev.name.trim() || undefined : undefined,
      id: prev.addressMode === "id" ? prev.id.trim() || undefined : undefined,
      sql: prev.sql,
      maxRows: 200,
    });
  }

  function handleBindingChange(next: DoBindingName) {
    setBinding(next);
    setAddressMode("name");
    if (next === "CONTROLLER") setTargetName(defaultCtrlName);
    else if (next === "COMMAND") setTargetName(statusInfo?.recentCommands[0]?.doName ?? "");
  }

  const columns = useMemo(() => {
    if (!runSql.data) return [];
    return runSql.data.columns.map((key) => {
      const lower = key.toLowerCase();
      const format =
        lower.endsWith("_at") || lower === "at" || lower === "last_seen" || lower === "t"
          ? ("timestamp" as const)
          : lower.includes("bytes") || lower === "size"
            ? ("bytes" as const)
            : lower.endsWith("_ms") || lower.includes("millis")
              ? ("durationMs" as const)
              : lower === "id" || lower.endsWith("_id")
                ? ("code" as const)
                : undefined;
      return { key, label: key, format };
    });
  }, [runSql.data]);

  const prevNavEntry = navHistory[navHistory.length - 1];
  const prevNavLabel = prevNavEntry
    ? formatDoTargetLabel({
        binding: prevNavEntry.binding,
        name: prevNavEntry.addressMode === "name" ? prevNavEntry.name : undefined,
        id: prevNavEntry.addressMode === "id" ? prevNavEntry.id : undefined,
      })
    : null;

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-12">
      {/* Left sidebar: target selector + schema tables */}
      <div className="space-y-3 lg:col-span-4">
        <div className="space-y-3 rounded-2xl border border-border bg-card/75 p-4">
          <div className="flex items-center justify-between gap-2">
            <div className="eyebrow text-[10.5px]">Target Cloudflare Durable Object</div>
            {prevNavLabel && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                className="h-6 max-w-44 gap-1 px-1.5 font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
                onClick={handleBack}
                title={`Back to ${prevNavLabel}`}
              >
                <ArrowLeft className="size-3 shrink-0" />
                <span className="truncate">{prevNavLabel}</span>
              </Button>
            )}
          </div>

          <div className="space-y-2">
            <Select
              items={bindingItems}
              value={binding}
              onValueChange={(v) => v && handleBindingChange(v as DoBindingName)}
            >
              <SelectTrigger className="h-8.5 w-full font-mono text-[12px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="font-mono text-[12px]">
                {bindingItems.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>

            <div className="flex gap-1.5">
              <Button
                type="button"
                size="sm"
                variant={addressMode === "name" ? "secondary" : "ghost"}
                className="h-7 flex-1 font-mono text-[11px]"
                onClick={() => setAddressMode("name")}
              >
                By Name
              </Button>
              <Button
                type="button"
                size="sm"
                variant={addressMode === "id" ? "secondary" : "ghost"}
                className="h-7 flex-1 font-mono text-[11px]"
                onClick={() => setAddressMode("id")}
              >
                By 64-Hex ID
              </Button>
            </div>

            {addressMode === "name" ? (
              <Input
                value={targetName}
                onChange={(e) => setTargetName(e.target.value)}
                placeholder={binding === "COMMAND" ? "cmd:<clusterId>:<commandId>" : defaultCtrlName}
                className="h-8.5 font-mono text-[12px]"
              />
            ) : (
              <Input
                value={targetId}
                onChange={(e) => setTargetId(e.target.value)}
                placeholder="64-character hex Cloudflare Durable Object ID"
                className="h-8.5 font-mono text-[11.5px]"
              />
            )}
          </div>

          {/* Quick targets */}
          {binding === "CONTROLLER" && statusInfo && statusInfo.controllers.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {statusInfo.controllers.map((c) => (
                <Button
                  key={c.name}
                  type="button"
                  size="sm"
                  variant="outline"
                  className="h-6 px-2 font-mono text-[10.5px]"
                  onClick={() => {
                    setAddressMode("name");
                    setTargetName(c.name);
                  }}
                >
                  {c.name}
                </Button>
              ))}
            </div>
          )}

          {binding === "COMMAND" && statusInfo && statusInfo.recentCommands.length > 0 && (
            <div className="space-y-1">
              <div className="font-mono text-[10px] text-faint uppercase">Recent commands</div>
              <div className="flex max-h-28 flex-wrap gap-1 overflow-y-auto">
                {statusInfo.recentCommands.map((c) => (
                  <Button
                    key={c.doName}
                    type="button"
                    size="sm"
                    variant="outline"
                    className="h-6 px-2 font-mono text-[10px]"
                    onClick={() => {
                      setAddressMode("name");
                      setTargetName(c.doName);
                    }}
                  >
                    {c.commandId.slice(0, 12)}
                  </Button>
                ))}
              </div>
            </div>
          )}
        </div>

        {/* Tables list */}
        <div className="rounded-2xl border border-border bg-card/75 p-4">
          <div className="mb-2 flex items-center justify-between gap-2">
            <span className="eyebrow text-[10.5px]">Tables</span>
            {schemaQuery.data && (
              <span className="num font-mono text-[10.5px] text-muted-foreground">
                {bytes(schemaQuery.data.databaseSizeBytes)}
              </span>
            )}
          </div>
          {schemaQuery.isLoading ? (
            <div className="flex items-center gap-2 py-4 font-mono text-[11.5px] text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" />
              Loading schema…
            </div>
          ) : schemaQuery.error ? (
            <div className="font-mono text-[11.5px] text-critical">{String(schemaQuery.error)}</div>
          ) : schemaQuery.data ? (
            <div className="max-h-96 space-y-1 overflow-y-auto">
              {schemaQuery.data.tables.filter((t) => !t.internal).length === 0 ? (
                <div className="py-3 font-mono text-[11.5px] text-muted-foreground">
                  No user tables found in this Cloudflare Durable Object.
                </div>
              ) : (
                schemaQuery.data.tables
                  .filter((t) => !t.internal)
                  .map((t) => (
                    <button
                      key={t.name}
                      type="button"
                      className="flex w-full items-center justify-between gap-2 rounded-lg px-2 py-1.5 text-left font-mono text-[11.5px] transition-colors hover:bg-secondary/70"
                      onClick={() => {
                        const q = `SELECT * FROM "${t.name}" LIMIT 50`;
                        setSql(q);
                        executeQuery(q);
                      }}
                    >
                      <span className="flex min-w-0 items-center gap-1.5">
                        <Table2 className="size-3.5 shrink-0 text-primary" />
                        <span className="truncate text-foreground">{t.name}</span>
                      </span>
                      <span className="num shrink-0 text-[10.5px] text-muted-foreground">
                        {t.rowCount ?? "—"}
                      </span>
                    </button>
                  ))
              )}
            </div>
          ) : null}
        </div>
      </div>

      {/* Right pane: SQL editor + formatted results */}
      <div className="space-y-4 lg:col-span-8">
        <div className="space-y-3 rounded-2xl border border-border bg-card/75 p-4">
          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-2 font-mono text-[11.5px] text-muted-foreground">
              <Database className="size-3.5 text-primary" />
              <span>Read-only SQLite query</span>
            </div>
            <Button
              type="button"
              size="sm"
              className="h-7.5 gap-1.5 font-mono text-[11px]"
              disabled={runSql.isPending}
              onClick={() => executeQuery()}
            >
              {runSql.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Play className="size-3.5" />}
              Run SQL
            </Button>
          </div>

          <Textarea
            value={sql}
            onChange={(e) => setSql(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                e.preventDefault();
                executeQuery();
              }
            }}
            rows={4}
            className="font-mono text-[12px]"
            placeholder="SELECT * FROM nodes LIMIT 50"
          />
        </div>

        {runSql.error && (
          <Alert variant="destructive" className="rounded-xl">
            <AlertTitle>SQL error</AlertTitle>
            <AlertDescription className="font-mono text-[12px]">{String(runSql.error)}</AlertDescription>
          </Alert>
        )}

        {runSql.data && (
          <div className="space-y-3">
            <SqlBlockView
              props={{
                sql: runSql.data.sql,
                target: runSql.data.target,
                rowCount: runSql.data.rowCount,
                rowsRead: runSql.data.rowsRead,
                durationMs: runSql.data.durationMs,
                truncated: runSql.data.truncated,
              }}
            />
            <DataTableView
              props={{
                title: runSql.data.target,
                columns,
                rows: runSql.data.rows,
                searchable: true,
              }}
              linkContext={{
                currentBinding: runSql.data.binding,
                currentTargetName: runSql.data.resolvedBy === "name" ? runSql.data.value : undefined,
                currentTargetId: runSql.data.resolvedBy === "id" ? runSql.data.value : undefined,
                sql: runSql.data.sql,
              }}
            />
          </div>
        )}
      </div>
    </div>
  );
}

export function AdminAgentPage() {
  const { data: statusInfo, error: statusErr } = useAdminAgentStatus();
  const [pendingSqlTarget, setPendingSqlTarget] = useState<{ target: DoTargetRef; seq: number } | null>(null);

  const handleOpenDoTarget = useCallback((target: DoTargetRef) => {
    setPendingSqlTarget((prev) => ({ target, seq: (prev?.seq ?? 0) + 1 }));
  }, []);

  const doLinkContextValue = useMemo(
    () => ({
      scope: statusInfo?.scope,
      allowedBindings: statusInfo?.bindings ?? (["CONTROLLER", "COMMAND"] as DoBindingName[]),
      onOpenDoTarget: handleOpenDoTarget,
    }),
    [handleOpenDoTarget, statusInfo?.bindings, statusInfo?.scope],
  );

  return (
    <div className="space-y-4">
      <PageTitle
        title="Admin Explorer"
        sub="Inspect schemas and run read-only SQL across this organization's Cloudflare Durable Objects"
      />

      {statusInfo && (
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline" className="gap-1.5 border-primary/35 bg-ember-wash font-mono text-[10.5px] text-primary">
            <DatabaseZap className="size-3" />
            Scope: {statusInfo.scope.orgId ?? statusInfo.scope.controllerDoName}
          </Badge>
          <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
            {statusInfo.bindings.length} Cloudflare Durable Object bindings
          </Badge>
          {statusInfo.gcWorkerConfigured && (
            <Badge variant="outline" className="border-good/40 font-mono text-[10.5px] text-good-ink">
              CladeFS GC: connected
            </Badge>
          )}
        </div>
      )}

      {statusErr && (
        <Alert variant="destructive" className="rounded-xl">
          <AlertTitle>Admin Explorer unavailable</AlertTitle>
          <AlertDescription className="font-mono text-[12px]">{String(statusErr)}</AlertDescription>
        </Alert>
      )}

      <DoLinkContext.Provider value={doLinkContextValue}>
        <SqlExplorerPanel
          statusInfo={statusInfo}
          pendingTarget={pendingSqlTarget}
          onConsumePendingTarget={() => setPendingSqlTarget(null)}
        />
      </DoLinkContext.Provider>
    </div>
  );
}
