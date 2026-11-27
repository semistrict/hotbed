import { useAgentChat } from "@cloudflare/ai-chat/react";
import { JSONUIProvider, Renderer, useJsonRenderMessage, type DataPart } from "@json-render/react";
import { useNavigate } from "@tanstack/react-router";
import { useAgent } from "agents/react";
import type { UIMessage } from "ai";
import {
  Bot,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Database,
  DatabaseZap,
  Loader2,
  Play,
  Plus,
  Send,
  Sparkles,
  Square,
  Table2,
  Trash2,
  Wrench,
  XCircle,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { adminAgentRegistry, DataTableView, SqlBlockView } from "../components/AdminAgentRegistry";
import { PageTitle } from "../components/PageTitle";
import {
  loadAdminThreads,
  newThreadId,
  saveAdminThreads,
  useAdminAgentStatus,
  useInspectDoSchema,
  useRunDoSql,
  type AdminAgentStatus,
  type AdminThreadEntry,
  type DoBindingName,
} from "../lib/adminAgent";
import { bytes } from "../lib/format";

const STARTER_PROMPTS = [
  {
    title: "Durable Object inventory",
    prompt:
      "Discover this organization's Cloudflare Durable Objects, show their SQLite database sizes, and break down the largest tables by row count.",
  },
  {
    title: "Recent command failures",
    prompt:
      "Run SQL on this organization's Controller Cloudflare Durable Object to find commands that failed in the last 24 hours, grouped by node and repo.",
  },
  {
    title: "Active sessions & workspaces",
    prompt:
      "Query this organization's Controller Cloudflare Durable Object for currently live sessions, workspaces, and connected nodes.",
  },
  {
    title: "CladeFS R2 storage & GC",
    prompt:
      "Check this organization's CladeFS tenant R2 object and byte usage across cells.",
  },
  {
    title: "Inspect latest Command object",
    prompt:
      "Find the most recent command in this organization's Controller Cloudflare Durable Object, then inspect its Command Cloudflare Durable Object schema and diagnostics.",
  },
] as const;

interface ExtractedToolPart {
  key: string;
  toolName: string;
  state: string;
  input?: unknown;
  output?: unknown;
  errorText?: string;
}

function extractToolParts(parts: UIMessage["parts"]): ExtractedToolPart[] {
  const out: ExtractedToolPart[] = [];
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i] as Record<string, unknown>;
    const type = typeof p.type === "string" ? p.type : "";
    if (type === "dynamic-tool") {
      out.push({
        key: String(p.toolCallId ?? i),
        toolName: String(p.toolName ?? "tool"),
        state: String(p.state ?? "unknown"),
        input: p.input,
        output: p.output,
        errorText: typeof p.errorText === "string" ? p.errorText : undefined,
      });
    } else if (type.startsWith("tool-")) {
      out.push({
        key: String(p.toolCallId ?? i),
        toolName: type.slice("tool-".length),
        state: String(p.state ?? "unknown"),
        input: p.input,
        output: p.output,
        errorText: typeof p.errorText === "string" ? p.errorText : undefined,
      });
    }
  }
  return out;
}

function ToolCallPill({ part }: { part: ExtractedToolPart }) {
  const [open, setOpen] = useState(false);
  const running = part.state === "input-streaming" || part.state === "input-available";
  const failed =
    part.state === "output-error" ||
    Boolean(part.errorText) ||
    Boolean(part.output && typeof part.output === "object" && "error" in (part.output as Record<string, unknown>) && (part.output as Record<string, unknown>).error);

  const inputObj = (part.input && typeof part.input === "object" ? part.input : null) as Record<string, unknown> | null;
  const outObj = (part.output && typeof part.output === "object" ? part.output : null) as Record<string, unknown> | null;

  const summary = useMemo(() => {
    if (part.toolName === "run_do_sql") {
      const target =
        typeof outObj?.target === "string"
          ? outObj.target
          : `${String(inputObj?.binding ?? "DO")}(${String(inputObj?.name ?? inputObj?.id ?? "default")})`;
      const rows = typeof outObj?.rowCount === "number" ? ` · ${outObj.rowCount} rows` : "";
      const dur = typeof outObj?.durationMs === "number" ? ` · ${outObj.durationMs}ms` : "";
      return `${target}${rows}${dur}`;
    }
    if (part.toolName === "inspect_do_schema") {
      const target =
        typeof outObj?.target === "string"
          ? outObj.target
          : `${String(inputObj?.binding ?? "DO")}(${String(inputObj?.name ?? inputObj?.id ?? "default")})`;
      const size = typeof outObj?.databaseSizeBytes === "number" ? ` · ${bytes(outObj.databaseSizeBytes)}` : "";
      return `${target}${size}`;
    }
    if (typeof inputObj?.path === "string") return inputObj.path;
    if (typeof inputObj?.action === "string") return inputObj.action;
    return "";
  }, [inputObj, outObj, part.toolName]);

  return (
    <div className="overflow-hidden rounded-lg border border-border/80 bg-secondary/35">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left font-mono text-[11px] text-muted-foreground transition-colors hover:bg-secondary/60 hover:text-foreground"
      >
        {open ? <ChevronDown className="size-3 shrink-0" /> : <ChevronRight className="size-3 shrink-0" />}
        {running ? (
          <Loader2 className="size-3 shrink-0 animate-spin text-primary" />
        ) : failed ? (
          <XCircle className="size-3 shrink-0 text-critical" />
        ) : (
          <CheckCircle2 className="size-3 shrink-0 text-good-ink" />
        )}
        <Wrench className="size-3 shrink-0 text-faint" />
        <span className="font-medium text-foreground">{part.toolName}</span>
        {summary && <span className="truncate text-muted-foreground">{summary}</span>}
      </button>
      {open && (
        <div className="space-y-2 border-t border-border/60 bg-background/50 p-2.5 font-mono text-[11px]">
          {typeof inputObj?.sql === "string" && (
            <div>
              <div className="mb-1 text-[10px] tracking-wide text-faint uppercase">SQL</div>
              <pre className="overflow-x-auto rounded bg-secondary/50 p-2 text-foreground">
                <code>{inputObj.sql}</code>
              </pre>
            </div>
          )}
          {part.input !== undefined && (
            <div>
              <div className="mb-1 text-[10px] tracking-wide text-faint uppercase">Arguments</div>
              <pre className="max-h-40 overflow-auto rounded bg-secondary/50 p-2 text-muted-foreground">
                <code>{JSON.stringify(part.input, null, 2)}</code>
              </pre>
            </div>
          )}
          {part.errorText && (
            <div className="rounded bg-[color-mix(in_oklab,var(--critical)_12%,transparent)] p-2 text-critical">
              {part.errorText}
            </div>
          )}
          {part.output !== undefined && (
            <div>
              <div className="mb-1 text-[10px] tracking-wide text-faint uppercase">Result</div>
              <pre className="max-h-56 overflow-auto rounded bg-secondary/50 p-2 text-muted-foreground">
                <code>{JSON.stringify(part.output, null, 2)}</code>
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function AssistantMessageContent({
  message,
  isStreaming,
  onFollowUp,
  onNavigate,
}: {
  message: UIMessage;
  isStreaming: boolean;
  onFollowUp: (prompt: string) => void;
  onNavigate: (path: string) => void;
}) {
  const { spec, text, hasSpec } = useJsonRenderMessage(message.parts as unknown as DataPart[]);
  const toolParts = useMemo(() => extractToolParts(message.parts), [message.parts]);

  const handlers = useMemo(
    () => ({
      askFollowUp: (params: Record<string, unknown>) => {
        if (typeof params.prompt === "string" && params.prompt.trim()) {
          onFollowUp(params.prompt.trim());
        }
      },
      navigateTo: (params: Record<string, unknown>) => {
        if (typeof params.path === "string" && params.path.startsWith("/")) {
          onNavigate(params.path);
        }
      },
      copyText: (params: Record<string, unknown>) => {
        if (typeof params.text === "string" && typeof navigator !== "undefined") {
          void navigator.clipboard?.writeText(params.text);
        }
      },
    }),
    [onFollowUp, onNavigate],
  );

  return (
    <div className="min-w-0 flex-1 space-y-3">
      {toolParts.length > 0 && (
        <div className="space-y-1.5">
          {toolParts.map((tp) => (
            <ToolCallPill key={tp.key} part={tp} />
          ))}
        </div>
      )}

      {text && (
        <div className="prose prose-sm max-w-none text-[13.5px] leading-relaxed text-foreground dark:prose-invert [&_code]:rounded [&_code]:bg-secondary/80 [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-[11.5px] [&_p]:my-1.5 [&_pre]:overflow-x-auto [&_pre]:rounded-xl [&_pre]:border [&_pre]:border-border [&_pre]:bg-secondary/50 [&_pre]:p-3 [&_ul]:my-1.5 [&_ul]:list-disc [&_ul]:pl-5">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
        </div>
      )}

      {hasSpec && spec && (
        <div className="pt-1">
          <JSONUIProvider registry={adminAgentRegistry} handlers={handlers}>
            <Renderer spec={spec} registry={adminAgentRegistry} loading={isStreaming} />
          </JSONUIProvider>
        </div>
      )}

      {isStreaming && !text && !hasSpec && toolParts.length === 0 && (
        <div className="flex items-center gap-2 font-mono text-[11.5px] text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin text-primary" />
          <span>Thinking…</span>
        </div>
      )}
    </div>
  );
}

function AgentChatPanel({
  threadId,
  selectedModel,
  statusInfo,
  pendingPrompt,
  onClearPendingPrompt,
}: {
  threadId: string;
  selectedModel: string;
  statusInfo: AdminAgentStatus | undefined;
  pendingPrompt: string | null;
  onClearPendingPrompt: () => void;
}) {
  const navigate = useNavigate();
  const [connected, setConnected] = useState(false);
  const [input, setInput] = useState("");
  const scrollEndRef = useRef<HTMLDivElement>(null);
  const modelRef = useRef(selectedModel);
  modelRef.current = selectedModel;

  const agent = useAgent({
    agent: "admin-agent",
    name: threadId,
    onOpen: () => setConnected(true),
    onClose: () => setConnected(false),
  });

  const { messages, sendMessage, clearHistory, stop, isStreaming, error } = useAgentChat({
    agent,
    prepareSendMessagesRequest: ({ messages: msgs, body }) => ({
      body: {
        ...body,
        messages: msgs,
        model: modelRef.current || undefined,
      },
    }),
  });

  const sendPrompt = useCallback(
    (promptText: string) => {
      const trimmed = promptText.trim();
      if (!trimmed) return;
      void sendMessage({
        role: "user",
        parts: [{ type: "text", text: trimmed }],
      });
    },
    [sendMessage],
  );

  useEffect(() => {
    if (pendingPrompt) {
      sendPrompt(pendingPrompt);
      onClearPendingPrompt();
    }
  }, [pendingPrompt, sendPrompt, onClearPendingPrompt]);

  useEffect(() => {
    scrollEndRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [messages.length, isStreaming]);

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!input.trim() || isStreaming) return;
    sendPrompt(input);
    setInput("");
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      if (!input.trim() || isStreaming) return;
      sendPrompt(input);
      setInput("");
    }
  }

  return (
    <div className="flex min-h-[600px] flex-col overflow-hidden rounded-2xl border border-border bg-card/65">
      {/* Chat status strip */}
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/80 bg-secondary/30 px-4 py-2">
        <div className="flex items-center gap-2 font-mono text-[11px] text-muted-foreground">
          <span
            className={`inline-block size-2 rounded-full ${connected ? "bg-good" : "bg-warning"}`}
          />
          <span>{connected ? "Connected to AdminAgent Cloudflare Durable Object" : "Connecting…"}</span>
          <span className="text-faint">·</span>
          <span className="text-foreground">thread: {threadId}</span>
        </div>
        {messages.length > 0 && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            className="h-7 gap-1.5 font-mono text-[11px] text-muted-foreground hover:text-critical"
            onClick={() => clearHistory()}
            disabled={isStreaming}
          >
            <Trash2 className="size-3.5" />
            Clear chat
          </Button>
        )}
      </div>

      {/* Messages */}
      <div className="flex-1 space-y-5 overflow-y-auto p-4 sm:p-5">
        {statusInfo && !statusInfo.aiConfigured && (
          <Alert variant="destructive" className="rounded-xl">
            <AlertTitle>No AI provider configured</AlertTitle>
            <AlertDescription className="text-[12px]">
              Bind Cloudflare Workers AI in <code>cf-controller/cloudflare.config.ts</code> or set{" "}
              <code>ANTHROPIC_API_KEY</code> / <code>OPENAI_API_KEY</code> as Worker secrets.
            </AlertDescription>
          </Alert>
        )}

        {error && (
          <Alert variant="destructive" className="rounded-xl">
            <AlertTitle>Agent error</AlertTitle>
            <AlertDescription className="font-mono text-[12px]">{error.message}</AlertDescription>
          </Alert>
        )}

        {messages.length === 0 ? (
          <div className="mx-auto flex max-w-2xl flex-col items-center py-8 text-center">
            <div className="mb-3 flex size-11 items-center justify-center rounded-2xl border border-primary/30 bg-ember-wash text-primary">
              <Sparkles className="size-5" />
            </div>
            <h3 className="text-[16px] font-semibold text-foreground">Ask anything about your Cloudflare Durable Objects</h3>
            <p className="mt-1 max-w-lg text-[13px] text-muted-foreground">
              The Admin Agent can discover every Controller, Tenant Registry, and Command Cloudflare Durable Object,
              inspect schemas, run read-only SQL, check CladeFS R2 storage, and format answers with interactive tables and charts.
            </p>
            <div className="mt-6 grid w-full grid-cols-1 gap-2.5 text-left sm:grid-cols-2">
              {STARTER_PROMPTS.map((item) => (
                <button
                  key={item.title}
                  type="button"
                  onClick={() => sendPrompt(item.prompt)}
                  className="group rounded-xl border border-border bg-card/80 p-3 text-left transition-colors hover:border-primary/45 hover:bg-ember-wash/40"
                >
                  <div className="text-[12.5px] font-medium text-foreground group-hover:text-primary">
                    {item.title}
                  </div>
                  <div className="mt-1 line-clamp-2 text-[11.5px] text-muted-foreground">{item.prompt}</div>
                </button>
              ))}
            </div>
          </div>
        ) : (
          messages.map((msg, idx) => {
            const isUser = msg.role === "user";
            const isLast = idx === messages.length - 1;
            const userText = msg.parts
              .filter((p): p is { type: "text"; text: string } => p.type === "text")
              .map((p) => p.text)
              .join("");
            return (
              <div key={msg.id} className={`flex gap-3 ${isUser ? "justify-end" : "justify-start"}`}>
                {!isUser && (
                  <div className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg border border-primary/30 bg-ember-wash text-primary">
                    <Bot className="size-4" />
                  </div>
                )}
                {isUser ? (
                  <div className="max-w-[82%] rounded-2xl rounded-tr-sm border border-primary/30 bg-ember-wash/80 px-3.5 py-2.5 text-[13.5px] whitespace-pre-wrap text-foreground">
                    {userText}
                  </div>
                ) : (
                  <AssistantMessageContent
                    message={msg}
                    isStreaming={isStreaming && isLast}
                    onFollowUp={(p) => sendPrompt(p)}
                    onNavigate={(path) => void navigate({ to: path })}
                  />
                )}
              </div>
            );
          })
        )}
        <div ref={scrollEndRef} />
      </div>

      {/* Composer */}
      <form onSubmit={onSubmit} className="border-t border-border/80 bg-card/90 p-3 sm:p-4">
        <div className="flex items-end gap-2">
          <Textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={onKeyDown}
            rows={2}
            placeholder="Ask a question or request a SQL query on any Cloudflare Durable Object… (Enter to send, Shift+Enter for newline)"
            className="min-h-[54px] resize-none bg-background/80 text-[13px]"
          />
          {isStreaming ? (
            <Button
              type="button"
              variant="destructive"
              size="icon"
              className="size-10 shrink-0 rounded-xl"
              onClick={() => void stop()}
              title="Stop generating"
            >
              <Square className="size-4" />
            </Button>
          ) : (
            <Button
              type="submit"
              size="icon"
              className="size-10 shrink-0 rounded-xl"
              disabled={!input.trim()}
              title="Send message"
            >
              <Send className="size-4" />
            </Button>
          )}
        </div>
      </form>
    </div>
  );
}

function SqlExplorerPanel({
  statusInfo,
  onAskAgent,
}: {
  statusInfo: AdminAgentStatus | undefined;
  onAskAgent: (prompt: string) => void;
}) {
  const defaultCtrlName = statusInfo?.scope.controllerDoName ?? "default";
  const allowedBindings = statusInfo?.bindings ?? (["CONTROLLER", "COMMAND", "ADMIN_AGENT"] as DoBindingName[]);
  const bindingItems = useMemo(() => {
    const labels: Record<DoBindingName, string> = {
      CONTROLLER: "CONTROLLER (ControllerDO)",
      TENANT_REGISTRY: "TENANT_REGISTRY (TenantRegistryDO)",
      COMMAND: "COMMAND (CommandDO)",
      ADMIN_AGENT: "ADMIN_AGENT (AdminAgentDO)",
    };
    return allowedBindings.map((b) => ({ value: b, label: labels[b] }));
  }, [allowedBindings]);

  const [binding, setBinding] = useState<DoBindingName>("CONTROLLER");
  const [addressMode, setAddressMode] = useState<"name" | "id">("name");
  const [targetName, setTargetName] = useState(defaultCtrlName);
  const [targetId, setTargetId] = useState("");
  const [sql, setSql] = useState(
    "SELECT name, type FROM sqlite_master WHERE type = 'table' ORDER BY name",
  );

  useEffect(() => {
    if (binding === "CONTROLLER") {
      setTargetName(defaultCtrlName);
    } else if (binding === "ADMIN_AGENT") {
      setTargetName(`admin:${defaultCtrlName}:main`);
    }
  }, [defaultCtrlName, binding]);

  const schemaQuery = useInspectDoSchema(
    {
      binding,
      name: addressMode === "name" ? targetName : undefined,
      id: addressMode === "id" ? targetId : undefined,
    },
    Boolean(addressMode === "name" ? targetName.trim() || binding === "CONTROLLER" || binding === "TENANT_REGISTRY" : targetId.trim().length === 64),
  );

  const runSql = useRunDoSql();

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

  function handleBindingChange(next: DoBindingName) {
    setBinding(next);
    setAddressMode("name");
    if (next === "TENANT_REGISTRY") setTargetName("global");
    else if (next === "CONTROLLER") setTargetName(defaultCtrlName);
    else if (next === "COMMAND") setTargetName(statusInfo?.recentCommands[0]?.doName ?? "");
    else if (next === "ADMIN_AGENT") setTargetName(`admin:${defaultCtrlName}:main`);
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

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-12">
      {/* Left sidebar: target selector + schema tables */}
      <div className="space-y-3 lg:col-span-4">
        <div className="space-y-3 rounded-2xl border border-border bg-card/75 p-4">
          <div className="eyebrow text-[10.5px]">Target Cloudflare Durable Object</div>

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
                placeholder={binding === "COMMAND" ? "cmd:<clusterId>:<commandId>" : "default"}
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
            <div className="flex items-center gap-2">
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="h-7.5 gap-1.5 font-mono text-[11px]"
                onClick={() =>
                  onAskAgent(
                    `Inspect ${binding} (${addressMode === "id" ? `id: ${targetId}` : `name: ${targetName}`}) and explain this query and its results:\n\`\`\`sql\n${sql}\n\`\`\``,
                  )
                }
              >
                <Sparkles className="size-3.5 text-primary" />
                Ask Agent
              </Button>
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
            />
          </div>
        )}
      </div>
    </div>
  );
}

export function AdminAgentPage() {
  const { data: statusInfo, error: statusErr } = useAdminAgentStatus();
  const scopeKey = statusInfo?.scope.controllerDoName ?? "default";
  const [threads, setThreads] = useState<AdminThreadEntry[]>(() => loadAdminThreads(scopeKey));
  const [activeThreadId, setActiveThreadId] = useState<string>(() => loadAdminThreads(scopeKey)[0]?.id ?? "main");
  const [selectedModel, setSelectedModel] = useState<string>("");
  const [activeTab, setActiveTab] = useState<string>("chat");
  const [pendingPrompt, setPendingPrompt] = useState<string | null>(null);

  useEffect(() => {
    const loaded = loadAdminThreads(scopeKey);
    setThreads(loaded);
    setActiveThreadId(loaded[0]?.id ?? "main");
  }, [scopeKey]);

  useEffect(() => {
    if (!selectedModel && statusInfo?.defaultModel) {
      setSelectedModel(statusInfo.defaultModel);
    }
  }, [selectedModel, statusInfo?.defaultModel]);

  function handleNewThread() {
    const id = newThreadId();
    const entry: AdminThreadEntry = {
      id,
      label: `Thread ${threads.length + 1}`,
      createdAt: Date.now(),
    };
    const next = [entry, ...threads];
    setThreads(next);
    saveAdminThreads(scopeKey, next);
    setActiveThreadId(id);
  }

  const modelItems = useMemo(
    () => (statusInfo?.models ?? []).map((m) => ({ value: m.id, label: m.label })),
    [statusInfo?.models],
  );

  return (
    <div className="space-y-4">
      <PageTitle
        title="Admin Agent"
        sub="Ask questions and run read-only SQL across this organization's Cloudflare Durable Objects, with formatted json-render output"
        right={
          <div className="flex flex-wrap items-center gap-2">
            {statusInfo && statusInfo.models.length > 0 && (
              <Select
                items={modelItems}
                value={selectedModel || statusInfo.defaultModel}
                onValueChange={(v) => v && setSelectedModel(v)}
              >
                <SelectTrigger className="h-8 w-60 bg-card/80 font-mono text-[11.5px]">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent className="font-mono text-[11.5px]">
                  {statusInfo.models.map((m) => (
                    <SelectItem key={m.id} value={m.id} className="text-[11.5px]">
                      {m.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="h-8 gap-1.5 font-mono text-[11.5px]"
              onClick={handleNewThread}
            >
              <Plus className="size-3.5" />
              New thread
            </Button>
          </div>
        }
      />

      {/* Capability status badges */}
      {statusInfo && (
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline" className="gap-1.5 border-primary/35 bg-ember-wash font-mono text-[10.5px] text-primary">
            <DatabaseZap className="size-3" />
            Scope: {statusInfo.scope.orgId ?? statusInfo.scope.controllerDoName}
          </Badge>
          <Badge variant="outline" className="font-mono text-[10.5px] text-muted-foreground">
            {statusInfo.bindings.length} Cloudflare Durable Object bindings
          </Badge>
          <Badge
            variant="outline"
            className={`font-mono text-[10.5px] ${statusInfo.workersAiBound ? "border-good/40 text-good-ink" : "text-muted-foreground"}`}
          >
            Workers AI: {statusInfo.workersAiBound ? "bound" : "off"}
          </Badge>
          {statusInfo.gcWorkerConfigured && (
            <Badge variant="outline" className="border-good/40 font-mono text-[10.5px] text-good-ink">
              CladeFS GC: connected
            </Badge>
          )}
          {statusInfo.scope.isGlobalAdmin && (
            <Badge
              variant="outline"
              className={`font-mono text-[10.5px] ${statusInfo.cloudflareApiConfigured ? "border-good/40 text-good-ink" : "text-muted-foreground"}`}
              title={
                statusInfo.cloudflareApiConfigured
                  ? "Account-wide Durable Object listing and Workers Observability enabled"
                  : "Set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID secrets to unlock account-wide listing and Workers Observability"
              }
            >
              Cloudflare REST API: {statusInfo.cloudflareApiConfigured ? "configured" : "optional (not set)"}
            </Badge>
          )}
        </div>
      )}

      {statusErr && (
        <Alert variant="destructive" className="rounded-xl">
          <AlertTitle>Admin Agent unavailable</AlertTitle>
          <AlertDescription className="font-mono text-[12px]">{String(statusErr)}</AlertDescription>
        </Alert>
      )}

      <Tabs value={activeTab} onValueChange={setActiveTab} className="space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <TabsList>
            <TabsTrigger value="chat" className="gap-1.5 font-mono text-[12px]">
              <Bot className="size-3.5" />
              Agent Chat
            </TabsTrigger>
            <TabsTrigger value="sql" className="gap-1.5 font-mono text-[12px]">
              <Database className="size-3.5" />
              SQL Explorer
            </TabsTrigger>
          </TabsList>

          {activeTab === "chat" && threads.length > 1 && (
            <div className="flex flex-wrap items-center gap-1">
              {threads.slice(0, 6).map((t) => (
                <Button
                  key={t.id}
                  type="button"
                  size="sm"
                  variant={t.id === activeThreadId ? "secondary" : "ghost"}
                  className="h-7 px-2.5 font-mono text-[11px]"
                  onClick={() => setActiveThreadId(t.id)}
                >
                  {t.label}
                </Button>
              ))}
            </div>
          )}
        </div>

        <TabsContent value="chat" className="mt-0">
          <AgentChatPanel
            key={`${scopeKey}:${activeThreadId}`}
            threadId={activeThreadId}
            selectedModel={selectedModel}
            statusInfo={statusInfo}
            pendingPrompt={pendingPrompt}
            onClearPendingPrompt={() => setPendingPrompt(null)}
          />
        </TabsContent>

        <TabsContent value="sql" className="mt-0">
          <SqlExplorerPanel
            statusInfo={statusInfo}
            onAskAgent={(prompt) => {
              setPendingPrompt(prompt);
              setActiveTab("chat");
            }}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
