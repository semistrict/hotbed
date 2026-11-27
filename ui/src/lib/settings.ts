import { create, fromJson, toJson } from "@bufbuild/protobuf";
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  api,
  type ManagedArtifact,
  type PutGitCredentialInput,
  type SetWorkerConfigInput,
  type StartGitHubLoginInput,
  type WorkerConfig,
  type WorkerConfigInput,
} from "../api";
import { WorkerConfigSchema } from "../gen/hotbed/v1/hotbed_pb";

export type TriState = "inherit" | "true" | "false";
export type ArchKey = "amd64" | "arm64";

export const ARCH_KEYS: readonly ArchKey[] = ["amd64", "arm64"];

export const BUILTIN_DEFAULTS = {
  freshen: {
    maxDutyCyclePercent: 5,
    minIntervalSeconds: 1800,
    maxCooldownSeconds: 21600,
    failureCooldownSeconds: 900,
    maxOptionalDurationSeconds: 1800,
    maxRequiredDurationSeconds: 3600,
    idleDelaySeconds: 15,
    disableOptional: false,
  },
  controller: {
    sessionIdleTimeoutSeconds: 3600,
    offlineGraceSeconds: 86400,
  },
  vm: {
    cacheType: "Unsafe",
    nofile: 65536,
  },
  system: {
    cladefsdBin: "/usr/local/bin/cladefsd",
    cladefsdService: "cladefsd.service",
    firecrackerBin: "/usr/local/bin/firecracker",
  },
} as const;

export interface ArtifactDraft {
  url: string;
  sha256: string;
  version: string;
  path: string;
}

export type ArtifactMapDraft = Record<ArchKey, ArtifactDraft>;

export interface SettingsDraft {
  // Runtime & systemd
  defaultRuntime: "" | "vm" | "cladefs";
  slice: string;
  unitPropertiesText: string;

  // Base freshening
  freshen: {
    disableOptional: TriState;
    maxDutyCyclePercent: string; // e.g. "5" for 0.05
    minIntervalSeconds: string;
    maxCooldownSeconds: string;
    failureCooldownSeconds: string;
    maxOptionalDurationSeconds: string;
    maxRequiredDurationSeconds: string;
    idleDelaySeconds: string;
  };

  // Org controller lifecycle (org defaults only)
  controller: {
    sessionIdleTimeoutSeconds: string;
    offlineGraceSeconds: string;
  };

  // Firecracker VM
  vm: {
    enabled: TriState;
    defaultRuntime: TriState;
    vcpus: string;
    memMib: string;
    diskBytes: string;
    nofile: string;
    cacheType: "" | "Unsafe" | "Writeback";
    disableDax: TriState;
    daxWindowBytes: string;
    daxPrivateMaxBytes: string;
    vmUser: string;
    bridgeName: string;
    bridgeCidr: string;
    dnsServersText: string;
    cladefsControlSock: string;
    cladefsVmSock: string;
    cladefsPeerAddr: string;
    cellRoot: string;
    firecrackerBin: string;
    kernelPath: string;
    guestdBin: string;
    kernels: ArtifactMapDraft;
    guestd: ArtifactMapDraft;
  };

  // CladeFS host-FUSE
  cladefsFuse: {
    enabled: TriState;
    defaultRuntime: TriState;
    cladefsControlSock: string;
    cladefsPeerAddr: string;
    cellRoot: string;
    mountRoot: string;
    mountStoresRoot: string;
    diskBytes: string;
  };

  // Host system binaries
  system: {
    cladefsdBin: string;
    cladefsdService: string;
    firecrackerBin: string;
    cladefsd: ArtifactMapDraft;
    firecracker: ArtifactMapDraft;
  };
}

function emptyArtifact(): ArtifactDraft {
  return { url: "", sha256: "", version: "", path: "" };
}

function artifactMapFromProto(map?: Record<string, ManagedArtifact>): ArtifactMapDraft {
  return {
    amd64: map?.amd64
      ? {
          url: map.amd64.url ?? "",
          sha256: map.amd64.sha256 ?? "",
          version: map.amd64.version ?? "",
          path: map.amd64.path ?? "",
        }
      : emptyArtifact(),
    arm64: map?.arm64
      ? {
          url: map.arm64.url ?? "",
          sha256: map.arm64.sha256 ?? "",
          version: map.arm64.version ?? "",
          path: map.arm64.path ?? "",
        }
      : emptyArtifact(),
  };
}

function triFromBool(v: boolean | undefined): TriState {
  if (v === true) return "true";
  if (v === false) return "false";
  return "inherit";
}

function boolFromTri(v: TriState): boolean | undefined {
  if (v === "true") return true;
  if (v === "false") return false;
  return undefined;
}

function numStr(v: number | bigint | undefined): string {
  return v === undefined ? "" : String(v);
}

function dutyPercentStr(v: number | undefined): string {
  if (v === undefined) return "";
  const pct = Math.round(v * 10000) / 100;
  return String(pct);
}

export function workerConfigToDraft(cfg?: WorkerConfig): SettingsDraft {
  const fr = cfg?.freshen;
  const ctrl = cfg?.controller;
  const vm = cfg?.vm;
  const fuse = cfg?.cladefsFuse;
  const sys = cfg?.system;
  const rt = cfg?.defaultRuntime === "vm" || cfg?.defaultRuntime === "cladefs" ? cfg.defaultRuntime : "";
  const ct = vm?.cacheType === "Unsafe" || vm?.cacheType === "Writeback" ? vm.cacheType : "";

  return {
    defaultRuntime: rt,
    slice: cfg?.slice ?? "",
    unitPropertiesText: (cfg?.unitProperties ?? []).join("\n"),
    freshen: {
      disableOptional: triFromBool(fr?.disableOptional),
      maxDutyCyclePercent: dutyPercentStr(fr?.maxDutyCycle),
      minIntervalSeconds: numStr(fr?.minIntervalSeconds),
      maxCooldownSeconds: numStr(fr?.maxCooldownSeconds),
      failureCooldownSeconds: numStr(fr?.failureCooldownSeconds),
      maxOptionalDurationSeconds: numStr(fr?.maxOptionalDurationSeconds),
      maxRequiredDurationSeconds: numStr(fr?.maxRequiredDurationSeconds),
      idleDelaySeconds: numStr(fr?.idleDelaySeconds),
    },
    controller: {
      sessionIdleTimeoutSeconds: numStr(ctrl?.sessionIdleTimeoutSeconds),
      offlineGraceSeconds: numStr(ctrl?.offlineGraceSeconds),
    },
    vm: {
      enabled: triFromBool(vm?.enabled),
      defaultRuntime: triFromBool(vm?.defaultRuntime),
      vcpus: numStr(vm?.vcpus),
      memMib: numStr(vm?.memMib),
      diskBytes: numStr(vm?.diskBytes),
      nofile: numStr(vm?.nofile),
      cacheType: ct,
      disableDax: triFromBool(vm?.disableDax),
      daxWindowBytes: numStr(vm?.daxWindowBytes),
      daxPrivateMaxBytes: numStr(vm?.daxPrivateMaxBytes),
      vmUser: vm?.vmUser ?? "",
      bridgeName: vm?.bridgeName ?? "",
      bridgeCidr: vm?.bridgeCidr ?? "",
      dnsServersText: (vm?.dnsServers ?? []).join(", "),
      cladefsControlSock: vm?.cladefsControlSock ?? "",
      cladefsVmSock: vm?.cladefsVmSock ?? "",
      cladefsPeerAddr: vm?.cladefsPeerAddr ?? "",
      cellRoot: vm?.cellRoot ?? "",
      firecrackerBin: vm?.firecrackerBin ?? "",
      kernelPath: vm?.kernelPath ?? "",
      guestdBin: vm?.guestdBin ?? "",
      kernels: artifactMapFromProto(vm?.kernels),
      guestd: artifactMapFromProto(vm?.guestd),
    },
    cladefsFuse: {
      enabled: triFromBool(fuse?.enabled),
      defaultRuntime: triFromBool(fuse?.defaultRuntime),
      cladefsControlSock: fuse?.cladefsControlSock ?? "",
      cladefsPeerAddr: fuse?.cladefsPeerAddr ?? "",
      cellRoot: fuse?.cellRoot ?? "",
      mountRoot: fuse?.mountRoot ?? "",
      mountStoresRoot: fuse?.mountStoresRoot ?? "",
      diskBytes: numStr(fuse?.diskBytes),
    },
    system: {
      cladefsdBin: sys?.cladefsdBin ?? "",
      cladefsdService: sys?.cladefsdService ?? "",
      firecrackerBin: sys?.firecrackerBin ?? "",
      cladefsd: artifactMapFromProto(sys?.cladefsd),
      firecracker: artifactMapFromProto(sys?.firecracker),
    },
  };
}

function parseOptionalInt(raw: string, label: string, min = 0): number | undefined {
  const s = raw.trim();
  if (!s) return undefined;
  const n = Number(s);
  if (!Number.isInteger(n) || n < min) {
    throw new Error(`${label} must be an integer >= ${min}`);
  }
  return n;
}

function parseOptionalBigInt(raw: string, label: string): bigint | undefined {
  const s = raw.trim();
  if (!s) return undefined;
  if (!/^\d+$/.test(s)) {
    throw new Error(`${label} must be a non-negative integer (bytes)`);
  }
  return BigInt(s);
}

function parseOptionalDutyPercent(raw: string): number | undefined {
  const s = raw.trim();
  if (!s) return undefined;
  const pct = Number(s);
  if (!Number.isFinite(pct) || pct <= 0 || pct > 100) {
    throw new Error("Max duty cycle must be between 0 (exclusive) and 100%");
  }
  return Math.round(pct * 10000) / 1_000_000;
}

function parseOptionalString(raw: string): string | undefined {
  const s = raw.trim();
  return s ? s : undefined;
}

function parseLines(raw: string): string[] {
  return raw
    .split(/\r?\n/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function parseCSV(raw: string): string[] {
  return raw
    .split(/[,\n]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function buildArtifactMap(draft: ArtifactMapDraft): Record<string, { url: string; sha256: string; version: string; path: string }> {
  const out: Record<string, { url: string; sha256: string; version: string; path: string }> = {};
  for (const arch of ARCH_KEYS) {
    const a = draft[arch];
    const url = a.url.trim();
    const sha256 = a.sha256.trim().toLowerCase();
    const version = a.version.trim();
    const path = a.path.trim();
    if (!url && !sha256 && !version && !path) continue;
    out[arch] = { url, sha256, version, path };
  }
  return out;
}

function hasAnyValue(obj: Record<string, unknown>): boolean {
  return Object.values(obj).some((v) => {
    if (v === undefined) return false;
    if (Array.isArray(v)) return v.length > 0;
    if (v && typeof v === "object") return Object.keys(v).length > 0;
    return true;
  });
}

/**
 * Converts UI form state to a clean WorkerConfigInput suitable for
 * SetWorkerConfig(..., replace: true). Never sets tenantId (which is
 * managed automatically per organization by the controller).
 */
export function draftToWorkerConfigInput(draft: SettingsDraft, opts: { isOrgDefaults: boolean }): WorkerConfigInput {
  const out: WorkerConfigInput = {};

  if (draft.defaultRuntime) out.defaultRuntime = draft.defaultRuntime;
  const slice = parseOptionalString(draft.slice);
  if (slice !== undefined) out.slice = slice;
  const unitProperties = parseLines(draft.unitPropertiesText);
  if (unitProperties.length > 0) out.unitProperties = unitProperties;

  const freshen: NonNullable<WorkerConfigInput["freshen"]> = {
    disableOptional: boolFromTri(draft.freshen.disableOptional),
    maxDutyCycle: parseOptionalDutyPercent(draft.freshen.maxDutyCyclePercent),
    minIntervalSeconds: parseOptionalInt(draft.freshen.minIntervalSeconds, "Min interval"),
    maxCooldownSeconds: parseOptionalInt(draft.freshen.maxCooldownSeconds, "Max cooldown"),
    failureCooldownSeconds: parseOptionalInt(draft.freshen.failureCooldownSeconds, "Failure backoff"),
    maxOptionalDurationSeconds: parseOptionalInt(draft.freshen.maxOptionalDurationSeconds, "Max optional duration"),
    maxRequiredDurationSeconds: parseOptionalInt(draft.freshen.maxRequiredDurationSeconds, "Max required duration"),
    idleDelaySeconds: parseOptionalInt(draft.freshen.idleDelaySeconds, "Idle settle delay"),
  };
  if (hasAnyValue(freshen as Record<string, unknown>)) {
    out.freshen = freshen;
  }

  if (opts.isOrgDefaults) {
    const controller: NonNullable<WorkerConfigInput["controller"]> = {
      sessionIdleTimeoutSeconds: parseOptionalInt(draft.controller.sessionIdleTimeoutSeconds, "Session idle timeout", 60),
      offlineGraceSeconds: parseOptionalInt(draft.controller.offlineGraceSeconds, "Offline node grace", 0),
    };
    if (hasAnyValue(controller as Record<string, unknown>)) {
      out.controller = controller;
    }
  }

  const vmKernels = buildArtifactMap(draft.vm.kernels);
  const vmGuestd = buildArtifactMap(draft.vm.guestd);
  const dnsServers = parseCSV(draft.vm.dnsServersText);
  const vm: NonNullable<WorkerConfigInput["vm"]> = {
    enabled: boolFromTri(draft.vm.enabled),
    defaultRuntime: boolFromTri(draft.vm.defaultRuntime),
    vcpus: parseOptionalInt(draft.vm.vcpus, "VM vCPUs"),
    memMib: parseOptionalInt(draft.vm.memMib, "VM memory (MiB)"),
    diskBytes: parseOptionalBigInt(draft.vm.diskBytes, "VM disk bytes"),
    nofile: parseOptionalBigInt(draft.vm.nofile, "VM RLIMIT_NOFILE"),
    cacheType: draft.vm.cacheType || undefined,
    disableDax: boolFromTri(draft.vm.disableDax),
    daxWindowBytes: parseOptionalBigInt(draft.vm.daxWindowBytes, "DAX window bytes"),
    daxPrivateMaxBytes: parseOptionalBigInt(draft.vm.daxPrivateMaxBytes, "DAX private max bytes"),
    vmUser: parseOptionalString(draft.vm.vmUser),
    bridgeName: parseOptionalString(draft.vm.bridgeName),
    bridgeCidr: parseOptionalString(draft.vm.bridgeCidr),
    dnsServers: dnsServers.length > 0 ? dnsServers : undefined,
    cladefsControlSock: parseOptionalString(draft.vm.cladefsControlSock),
    cladefsVmSock: parseOptionalString(draft.vm.cladefsVmSock),
    cladefsPeerAddr: parseOptionalString(draft.vm.cladefsPeerAddr),
    cellRoot: parseOptionalString(draft.vm.cellRoot),
    firecrackerBin: parseOptionalString(draft.vm.firecrackerBin),
    kernelPath: parseOptionalString(draft.vm.kernelPath),
    guestdBin: parseOptionalString(draft.vm.guestdBin),
    kernels: Object.keys(vmKernels).length > 0 ? vmKernels : undefined,
    guestd: Object.keys(vmGuestd).length > 0 ? vmGuestd : undefined,
  };
  if (hasAnyValue(vm as Record<string, unknown>)) {
    out.vm = vm;
  }

  const cladefsFuse: NonNullable<WorkerConfigInput["cladefsFuse"]> = {
    enabled: boolFromTri(draft.cladefsFuse.enabled),
    defaultRuntime: boolFromTri(draft.cladefsFuse.defaultRuntime),
    cladefsControlSock: parseOptionalString(draft.cladefsFuse.cladefsControlSock),
    cladefsPeerAddr: parseOptionalString(draft.cladefsFuse.cladefsPeerAddr),
    cellRoot: parseOptionalString(draft.cladefsFuse.cellRoot),
    mountRoot: parseOptionalString(draft.cladefsFuse.mountRoot),
    mountStoresRoot: parseOptionalString(draft.cladefsFuse.mountStoresRoot),
    diskBytes: parseOptionalBigInt(draft.cladefsFuse.diskBytes, "FUSE disk bytes"),
  };
  if (hasAnyValue(cladefsFuse as Record<string, unknown>)) {
    out.cladefsFuse = cladefsFuse;
  }

  const sysCladefsd = buildArtifactMap(draft.system.cladefsd);
  const sysFc = buildArtifactMap(draft.system.firecracker);
  const system: NonNullable<WorkerConfigInput["system"]> = {
    cladefsdBin: parseOptionalString(draft.system.cladefsdBin),
    cladefsdService: parseOptionalString(draft.system.cladefsdService),
    firecrackerBin: parseOptionalString(draft.system.firecrackerBin),
    cladefsd: Object.keys(sysCladefsd).length > 0 ? sysCladefsd : undefined,
    firecracker: Object.keys(sysFc).length > 0 ? sysFc : undefined,
  };
  if (hasAnyValue(system as Record<string, unknown>)) {
    out.system = system;
  }

  return out;
}

/**
 * Mirrors worker's RequiresConfigRestart (internal/worker/config_sync.go):
 * indicates whether saving the diff between baseline and next changes
 * structural runtime fields (requiring an idle-deferred worker restart) vs
 * hot-reloading immediately in place. Sub-fields of the VM and FUSE sections
 * only matter when both sides enable the section. The draft carries neither
 * tenantId (the UI never sets it) nor the machined fields (not in the proto).
 */
export function draftRequiresRestart(prev: SettingsDraft, next: SettingsDraft): boolean {
  if (prev.slice.trim() !== next.slice.trim()) return true;
  const prevUnits = parseLines(prev.unitPropertiesText);
  const nextUnits = parseLines(next.unitPropertiesText);
  if (prevUnits.length !== nextUnits.length || prevUnits.some((u, i) => u !== nextUnits[i])) return true;

  const prevVm = prev.vm.enabled === "true";
  const nextVm = next.vm.enabled === "true";
  if (prevVm !== nextVm) return true;
  if (prevVm && nextVm && differsTrimmed(prev.vm, next.vm, VM_STRUCTURAL_KEYS)) return true;

  const prevFuse = prev.cladefsFuse.enabled === "true";
  const nextFuse = next.cladefsFuse.enabled === "true";
  if (prevFuse !== nextFuse) return true;
  if (prevFuse && nextFuse && differsTrimmed(prev.cladefsFuse, next.cladefsFuse, FUSE_STRUCTURAL_KEYS)) {
    return true;
  }
  return false;
}

const VM_STRUCTURAL_KEYS = ["cladefsControlSock", "cladefsVmSock", "cellRoot", "bridgeName", "bridgeCidr"] as const;
const FUSE_STRUCTURAL_KEYS = ["cladefsControlSock", "cellRoot", "mountRoot", "mountStoresRoot"] as const;

function differsTrimmed<T extends object, K extends keyof T>(a: T, b: T, keys: readonly K[]): boolean {
  return keys.some((k) => String(a[k]).trim() !== String(b[k]).trim());
}

/** Which saved config (scope and its content signature) a settings draft was last reset to. */
export interface DraftSync {
  scope: string;
  sig: string;
}

/**
 * What the settings draft does when the saved config it follows may have moved:
 * "current" (nothing moved), "hold" (the saved config changed under unsaved
 * edits: keep them and offer a reload) or "reset" (follow the saved config).
 * Switching scope always resets; a background refetch never wipes edits.
 * `hasUnsavedEdits` means edits to the draft since it last followed the saved
 * config (not "differs from the new saved config", which a refetch alone makes true).
 */
export function draftSyncAction(synced: DraftSync, current: DraftSync, hasUnsavedEdits: boolean): "current" | "hold" | "reset" {
  if (synced.scope !== current.scope) return "reset";
  if (synced.sig === current.sig) return "current";
  return hasUnsavedEdits ? "hold" : "reset";
}

export function workerConfigToPrettyJson(cfg?: WorkerConfig | WorkerConfigInput): string {
  if (!cfg) return "{}\n";
  const msg = create(WorkerConfigSchema, cfg as WorkerConfigInput);
  const raw = toJson(WorkerConfigSchema, msg) as Record<string, unknown>;
  // Omit read-only metadata fields so the JSON is directly pasteable to `hotbed config set`.
  delete raw.node;
  delete raw.revision;
  delete raw.updatedAt;
  delete raw.updatedBy;
  if (raw.vm && typeof raw.vm === "object") {
    delete (raw.vm as Record<string, unknown>).tenantId;
  }
  if (raw.cladefsFuse && typeof raw.cladefsFuse === "object") {
    delete (raw.cladefsFuse as Record<string, unknown>).tenantId;
  }
  return `${JSON.stringify(raw, null, 2)}\n`;
}

export function parseWorkerConfigJson(raw: string): WorkerConfigInput {
  const parsed: unknown = JSON.parse(raw);
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("Config JSON must be an object");
  }
  const copy = { ...(parsed as Record<string, unknown>) };
  delete copy.node;
  delete copy.revision;
  delete copy.updatedAt;
  delete copy.updatedBy;
  const msg = fromJson(WorkerConfigSchema, copy as Parameters<typeof fromJson>[1]);
  if (msg.vm) msg.vm.tenantId = undefined;
  if (msg.cladefsFuse) msg.cladefsFuse.tenantId = undefined;
  return msg;
}

export function formatDurationSeconds(sec: number): string {
  if (!Number.isFinite(sec) || sec < 0) return "—";
  if (sec === 0) return "disabled";
  if (sec % 86400 === 0) return `${sec / 86400}d`;
  if (sec % 3600 === 0) return `${sec / 3600}h`;
  if (sec % 60 === 0) return `${sec / 60}m`;
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return `${m}m ${s}s`;
}

export function formatSecondsHint(raw: string, fallbackSeconds?: number): string {
  const s = raw.trim();
  if (!s) {
    return fallbackSeconds !== undefined ? `default: ${formatDurationSeconds(fallbackSeconds)}` : "inherited";
  }
  const n = Number(s);
  if (!Number.isFinite(n) || n < 0) return "invalid";
  return formatDurationSeconds(n);
}

export function formatBytesHint(raw: string): string {
  const s = raw.trim();
  if (!s) return "inherited";
  if (!/^\d+$/.test(s)) return "invalid";
  const n = Number(s);
  if (n === 0) return "0 (default)";
  const gib = n / 1024 ** 3;
  if (gib >= 1) return `${gib.toFixed(gib >= 10 ? 0 : 1)} GiB`;
  const mib = n / 1024 ** 2;
  if (mib >= 1) return `${mib.toFixed(0)} MiB`;
  return `${n} B`;
}

export function draftToCliCommand(scope: string, input: WorkerConfigInput): string {
  const json = workerConfigToPrettyJson(input).trim();
  const target = scope ? ` ${scope}` : "";
  return `hotbed config set${target} --replace <<'EOF'\n${json}\nEOF`;
}

export function useWorkerConfig(node = "") {
  return useQuery({
    queryKey: ["worker-config", node],
    queryFn: () => api.getWorkerConfig(node),
    placeholderData: keepPreviousData,
  });
}

export function useSetWorkerConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: SetWorkerConfigInput) => api.setWorkerConfig(input),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["worker-config"] });
    },
  });
}

export function useDeleteWorkerConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { node?: string; defaults?: boolean }) => api.deleteWorkerConfig(input),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["worker-config"] });
    },
  });
}

export function useUpgradeSystem() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (node: string) => api.upgradeSystem(node),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["snapshot"] });
    },
  });
}

export function useGitCredentials(host = "") {
  return useQuery({
    queryKey: ["git-credentials", host],
    queryFn: () => api.listGitCredentials(host),
  });
}

export function usePutGitCredential() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: PutGitCredentialInput) => api.putGitCredential(input),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["git-credentials"] });
    },
  });
}

export function useDeleteGitCredential() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteGitCredential(name),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["git-credentials"] });
    },
  });
}

export function useStartGitHubLogin() {
  return useMutation({
    mutationFn: (input?: StartGitHubLoginInput) => api.startGitHubLogin(input),
  });
}

export function useCompleteGitHubLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (loginId: string) => api.completeGitHubLogin(loginId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["git-credentials"] });
    },
  });
}
