import {
  BUILTIN_DEFAULTS,
  formatBytesHint,
  formatSecondsHint,
  type ArtifactPath,
  type FieldPath,
  type SettingsDraft,
} from "./settings";

/**
 * How a field is edited and checked: free text, a whole number, a byte count,
 * a duration in seconds, a percentage, inherit/on/off, one of a few options,
 * or one entry per line.
 */
export type FieldKind = "text" | "int" | "bytes" | "seconds" | "percent" | "tri" | "enum" | "lines";

export interface FieldOption {
  /** "" stands for inheriting the lower layer. */
  value: string;
  label: string;
}

/** One field of the worker config form. */
export interface FieldSpec {
  path: FieldPath;
  /** The DOM id, which also names the field in tests. */
  id: string;
  label: string;
  kind: FieldKind;
  /** Shown in an empty field when nothing is inherited: the built-in default or an example. */
  placeholder?: string;
  /** For seconds fields: the built-in default, which the hint names while the field is empty. */
  defaultSeconds?: number;
  /** A fixed description: after the computed hint of a seconds field, else when nothing inherited is shown. */
  note?: string;
  /** Show "Org default: X" while a node override inherits a value. */
  orgHint?: boolean;
  /** The unit that follows an inherited value in its hint. */
  unit?: string;
  /** A hint worked out from what is typed (and inherited), which wins when it gives one. */
  preview?: (value: string, inherited: string) => string | undefined;
  /** The lowest number a numeric field takes (0 by default). */
  min?: number;
  options?: readonly FieldOption[];
  /** For tri fields: the labels of the two set states, and the words for each state in the inherit label. */
  tri?: { trueLabel?: string; falseLabel?: string; on?: string; off?: string };
}

/** A card of fields. */
export interface SectionSpec {
  title: string;
  sub: string;
  columns: 2 | 3;
  fields: readonly FieldSpec[];
}

/** A card of per-arch managed artifacts. */
export interface ArtifactSpec {
  path: ArtifactPath;
  title: string;
  sub: string;
}

export const RUNTIME_OPTIONS: readonly FieldOption[] = [
  { value: "", label: "Inherit / default" },
  { value: "vm", label: "vm (Firecracker microVM)" },
  { value: "cladefs", label: "cladefs (host FUSE)" },
];

export const CACHE_TYPE_OPTIONS: readonly FieldOption[] = [
  { value: "", label: "Inherit (Unsafe)" },
  { value: "Unsafe", label: "Unsafe (non-blocking fsync)" },
  { value: "Writeback", label: "Writeback" },
];

const fr = BUILTIN_DEFAULTS.freshen;
const ctl = BUILTIN_DEFAULTS.controller;
const sys = BUILTIN_DEFAULTS.system;

export const FRESHEN_SECTION: SectionSpec = {
  title: "Base Freshening Caps",
  sub: "Bounds commit-only base freshening so it stays a lightweight optimization. Structural changes (cold builds, setup step edits, project rebuilds) always run immediately. Hot-reloads with zero restart.",
  columns: 3,
  fields: [
    {
      path: ["freshen", "disableOptional"],
      id: "freshen-disable-optional",
      label: "Optional commit-only freshens",
      kind: "tri",
      note: "When paused, nodes only freshen on cold start, step/repo edits, or `hotbed project rebuild`.",
      tri: { trueLabel: "Paused (disable optional)", falseLabel: "Active (capped)", on: "paused", off: "active" },
    },
    {
      path: ["freshen", "maxDutyCyclePercent"],
      id: "freshen-duty",
      label: "Max duty cycle (%)",
      kind: "percent",
      placeholder: String(fr.maxDutyCyclePercent),
      preview: dutyPreview,
    },
    seconds(["freshen", "minIntervalSeconds"], "freshen-min-interval", "Min interval per project (s)", fr.minIntervalSeconds),
    seconds(["freshen", "maxCooldownSeconds"], "freshen-max-cooldown", "Max cooldown cap (s)", fr.maxCooldownSeconds),
    seconds(
      ["freshen", "failureCooldownSeconds"],
      "freshen-failure-backoff",
      "Failure exponential backoff (s)",
      fr.failureCooldownSeconds,
      "doubles per failure",
    ),
    seconds(
      ["freshen", "idleDelaySeconds"],
      "freshen-idle-delay",
      "Idle settle delay (s)",
      fr.idleDelaySeconds,
      "quiet before background (-10)",
    ),
    seconds(
      ["freshen", "maxOptionalDurationSeconds"],
      "freshen-opt-limit",
      "Optional hard limit (s)",
      fr.maxOptionalDurationSeconds,
      "0 = unlimited",
    ),
    seconds(
      ["freshen", "maxRequiredDurationSeconds"],
      "freshen-req-limit",
      "Required hard limit (s)",
      fr.maxRequiredDurationSeconds,
      "cold/setup/rebuild",
    ),
  ],
};

/** Org-wide only: a node override shows these read-only. */
export const CONTROLLER_SECTION: SectionSpec = {
  title: "Controller & Session Lifecycle",
  sub: "Per-organization session reaping timeouts enforced by this organization's Cloudflare Durable Object.",
  columns: 2,
  fields: [
    {
      ...seconds(
        ["controller", "sessionIdleTimeoutSeconds"],
        "ctrl-session-idle",
        "Session idle timeout (s)",
        ctl.sessionIdleTimeoutSeconds,
        "minimum 60s",
      ),
      min: 60,
    },
    seconds(
      ["controller", "offlineGraceSeconds"],
      "ctrl-offline-grace",
      "Offline node grace period (s)",
      ctl.offlineGraceSeconds,
      "wait before reaping sessions on an offline node",
    ),
  ],
};

export const RUNTIME_SECTION: SectionSpec = {
  title: "Runtime & Systemd Defaults",
  sub: "Controls which workspace runtime non-bench runs select by default and how worker systemd units are configured.",
  columns: 2,
  fields: [
    {
      path: ["defaultRuntime"],
      id: "rt-default",
      label: "Default workspace runtime",
      kind: "enum",
      options: RUNTIME_OPTIONS,
      orgHint: true,
      note: "Used for non-bench workspaces when --runtime is omitted",
    },
    {
      path: ["slice"],
      id: "rt-slice",
      label: "Systemd slice",
      kind: "text",
      orgHint: true,
      note: "e.g. hotbed.slice or user-1000-hotbed.slice",
      placeholder: "hotbed.slice",
    },
    {
      path: ["unitPropertiesText"],
      id: "rt-unit-props",
      label: "Systemd unit properties",
      kind: "lines",
      note: "One Key=Value property per line (replaces lower layer when non-empty).",
      placeholder: "TasksMax=4096\nLimitNOFILE=65536",
    },
  ],
};

export const VM_COMPUTE_SECTION: SectionSpec = {
  title: "Firecracker VM Compute & Lifecycle",
  sub: "Sizing and idle hibernation for microVM workspaces. Sizing changes hot-reload for newly started VMs; enabling/disabling VM mode triggers an idle-deferred restart.",
  columns: 3,
  fields: [
    { path: ["vm", "enabled"], id: "vm-enabled", label: "VM runtime enabled", kind: "tri" },
    { path: ["vm", "defaultRuntime"], id: "vm-default-rt", label: "Use VM as default runtime", kind: "tri" },
    {
      path: ["vm", "vcpus"],
      id: "vm-vcpus",
      label: "vCPUs per VM",
      kind: "int",
      orgHint: true,
      note: "0 = worker default",
      placeholder: "8",
    },
    {
      path: ["vm", "memMib"],
      id: "vm-mem",
      label: "Memory per VM (MiB)",
      kind: "int",
      orgHint: true,
      unit: "MiB",
      note: "e.g. 8192 or 12288",
      placeholder: "8192",
      preview: (v) => (Number(v) > 0 ? `${(Number(v) / 1024).toFixed(1)} GiB` : undefined),
    },
    {
      path: ["vm", "nofile"],
      id: "vm-nofile",
      label: "Guest RLIMIT_NOFILE",
      kind: "int",
      note: `default: ${BUILTIN_DEFAULTS.vm.nofile}`,
      placeholder: String(BUILTIN_DEFAULTS.vm.nofile),
    },
    bytes(["vm", "diskBytes"], "vm-disk", "Rootfs disk size (bytes)", "e.g. 68719476736 (64 GiB)"),
    { path: ["vm", "cacheType"], id: "vm-cache-type", label: "virtio-extent cache type", kind: "enum", options: CACHE_TYPE_OPTIONS },
    {
      path: ["vm", "disableDax"],
      id: "vm-disable-dax",
      label: "Disable DAX window",
      kind: "tri",
      note: "Needed on 16 KiB Apple Silicon hosts.",
      tri: { trueLabel: "Disable DAX", falseLabel: "Allow DAX" },
    },
    bytes(["vm", "daxWindowBytes"], "vm-dax-window", "DAX window (bytes)", "e.g. 17179869184 (16 GiB)"),
    bytes(["vm", "daxPrivateMaxBytes"], "vm-dax-private", "DAX private max (bytes)", "bytes"),
    text(["vm", "vmUser"], "vm-user", "VM command user", "hotbed"),
  ],
};

export const VM_HOST_SECTION: SectionSpec = {
  title: "VM Networking, Sockets & Host Paths",
  sub: "Low-level bridge, CladeFS daemon socket, and host binary paths. Usually left to local hotbed.json unless overridden per node.",
  columns: 3,
  fields: [
    text(["vm", "bridgeName"], "vm-bridge", "Bridge name", "hotbed0"),
    text(["vm", "bridgeCidr"], "vm-cidr", "Bridge CIDR", "10.200.0.1/24"),
    text(["vm", "dnsServersText"], "vm-dns", "DNS servers (comma-separated)", "1.1.1.1, 8.8.8.8"),
    text(["vm", "cladefsPeerAddr"], "vm-peer-addr", "CladeFS peer address", "100.x.y.z:9185"),
    text(["vm", "cladefsControlSock"], "vm-ctrl-sock", "CladeFS control socket", "/run/cladefsd/control.sock"),
    text(["vm", "cladefsVmSock"], "vm-vm-sock", "CladeFS VM socket", "/run/cladefsd/vm.sock"),
    text(["vm", "cellRoot"], "vm-cell-root", "Cell root", "/var/lib/cladefs/cells"),
    text(["vm", "firecrackerBin"], "vm-fc-bin", "Firecracker binary path", "/usr/local/bin/firecracker"),
    text(["vm", "kernelPath"], "vm-kernel-path", "Local kernel path", "/var/lib/hotbed/vmlinux"),
    text(["vm", "guestdBin"], "vm-guestd-bin", "Local guestd path", "/usr/local/bin/hotbed-guestd"),
  ],
};

export const VM_ARTIFACTS: readonly ArtifactSpec[] = [
  {
    path: ["vm", "kernels"],
    title: "Managed Guest Kernels (vmlinux)",
    sub: "SHA-256 verified guest kernels downloaded automatically by workers per architecture.",
  },
  {
    path: ["vm", "guestd"],
    title: "Managed hotbed-guestd Overrides",
    sub: "Optional per-arch guestd override. Leave unset to follow each controller release automatically.",
  },
];

export const FUSE_SECTION: SectionSpec = {
  title: "CladeFS Host-FUSE Runtime",
  sub: "Used on non-KVM hosts (e.g. nested VMs or --bench bare-metal runs) with R2-backed cells mounted via cladefsd.",
  columns: 3,
  fields: [
    { path: ["cladefsFuse", "enabled"], id: "fuse-enabled", label: "Host-FUSE enabled", kind: "tri" },
    { path: ["cladefsFuse", "defaultRuntime"], id: "fuse-default-rt", label: "Use FUSE as default runtime", kind: "tri" },
    bytes(["cladefsFuse", "diskBytes"], "fuse-disk", "Cell disk size (bytes)", "bytes"),
    text(["cladefsFuse", "cladefsPeerAddr"], "fuse-peer", "CladeFS peer address", "100.x.y.z:9185"),
    text(["cladefsFuse", "cladefsControlSock"], "fuse-ctrl-sock", "CladeFS control socket", "/run/cladefsd/control.sock"),
    text(["cladefsFuse", "cellRoot"], "fuse-cell-root", "Cell root", "/var/lib/cladefs/cells"),
    text(["cladefsFuse", "mountRoot"], "fuse-mount-root", "Mount root", "/var/lib/hotbed/worker/fuse-mnt"),
    text(["cladefsFuse", "mountStoresRoot"], "fuse-mount-stores", "Mount stores root", "/var/lib/hotbed/worker/fuse-stores"),
  ],
};

export const SYSTEM_SECTION: SectionSpec = {
  title: "Host Paths & Service",
  sub: "Applied only when you explicitly trigger a system upgrade (restarting cladefsd terminates live VMs and FUSE mounts).",
  columns: 3,
  fields: [
    text(["system", "cladefsdBin"], "sys-cladefsd-bin", "cladefsd install path", sys.cladefsdBin),
    text(["system", "cladefsdService"], "sys-cladefsd-svc", "cladefsd systemd service", sys.cladefsdService),
    text(["system", "firecrackerBin"], "sys-fc-bin", "firecracker install path", sys.firecrackerBin),
  ],
};

export const SYSTEM_ARTIFACTS: readonly ArtifactSpec[] = [
  {
    path: ["system", "cladefsd"],
    title: "Managed cladefsd Binaries",
    sub: "Staged and SHA-256 verified before restarting cladefsd during system upgrade.",
  },
  {
    path: ["system", "firecracker"],
    title: "Managed Firecracker Binaries",
    sub: "Installed atomically to firecracker_bin during system upgrade.",
  },
];

export const ALL_SECTIONS: readonly SectionSpec[] = [
  FRESHEN_SECTION,
  CONTROLLER_SECTION,
  RUNTIME_SECTION,
  VM_COMPUTE_SECTION,
  VM_HOST_SECTION,
  FUSE_SECTION,
  SYSTEM_SECTION,
];

/** One-click freshen settings: each replaces the fields it names. */
export const FRESHEN_PRESETS: readonly { label: string; freshen: Partial<SettingsDraft["freshen"]> }[] = [
  {
    label: "Balanced (5% / 30m)",
    freshen: {
      disableOptional: "false",
      maxDutyCyclePercent: String(fr.maxDutyCyclePercent),
      minIntervalSeconds: String(fr.minIntervalSeconds),
      maxCooldownSeconds: String(fr.maxCooldownSeconds),
      failureCooldownSeconds: String(fr.failureCooldownSeconds),
      maxOptionalDurationSeconds: String(fr.maxOptionalDurationSeconds),
      maxRequiredDurationSeconds: String(fr.maxRequiredDurationSeconds),
      idleDelaySeconds: String(fr.idleDelaySeconds),
    },
  },
  {
    label: "Conservative (2% / 60m)",
    freshen: {
      disableOptional: "false",
      maxDutyCyclePercent: "2",
      minIntervalSeconds: "3600",
      maxCooldownSeconds: "21600",
      idleDelaySeconds: "60",
    },
  },
  { label: "Pause optional", freshen: { disableOptional: "true" } },
];

/** What an empty field shows: the value it inherits, else the spec's default or example. */
export function fieldPlaceholder(spec: FieldSpec, inherited: string): string | undefined {
  if (inherited) {
    if (spec.kind === "seconds") return `Inherit (${inherited}s)`;
    if (spec.kind === "percent") return `Inherit (${inherited}%)`;
    return inherited;
  }
  return spec.placeholder ?? (spec.defaultSeconds !== undefined ? String(spec.defaultSeconds) : undefined);
}

/** The line under a field: what its value means, what it inherits, or what it is for. */
export function fieldHint(spec: FieldSpec, value: string, inherited: string): string | undefined {
  const preview = spec.preview?.(value, inherited);
  if (preview) return preview;
  if (spec.kind === "seconds") {
    const hint = formatSecondsHint(value, inherited ? Number(inherited) : spec.defaultSeconds);
    return spec.note ? `${hint} · ${spec.note}` : hint;
  }
  if (spec.kind === "bytes") return formatBytesHint(value);
  if (spec.orgHint && inherited) return `Org default: ${inherited}${spec.unit ? ` ${spec.unit}` : ""}`;
  return spec.note;
}

/** The inherit choice of a tri field, naming what it falls back to (nothing set means off). */
export function triInheritLabel(spec: FieldSpec, inherited: string): string {
  const on = spec.tri?.on ?? "enabled";
  const off = spec.tri?.off ?? "disabled";
  if (inherited === "true") return `Inherit (${on})`;
  if (inherited === "false") return `Inherit (${off})`;
  return `Default (${off})`;
}

/** The preview under the duty-cycle field: how long a minute of freshening cools down for. */
function dutyPreview(value: string, inherited: string): string {
  const pct = Number(value.trim() || inherited || fr.maxDutyCyclePercent);
  if (!Number.isFinite(pct) || pct <= 0 || pct > 100) return "valid range: (0, 100]%";
  const coolPerMin = 100 / pct - 1;
  const cool = coolPerMin >= 1 ? `${coolPerMin.toFixed(coolPerMin < 10 ? 1 : 0)}m` : `${Math.round(coolPerMin * 60)}s`;
  return `${pct}% cap · 1m active freshen → ${cool} cooldown`;
}

function seconds(path: FieldPath, id: string, label: string, defaultSeconds: number, note?: string): FieldSpec {
  return { path, id, label, kind: "seconds", defaultSeconds, note };
}

function bytes(path: FieldPath, id: string, label: string, placeholder: string): FieldSpec {
  return { path, id, label, kind: "bytes", placeholder };
}

function text(path: FieldPath, id: string, label: string, placeholder: string): FieldSpec {
  return { path, id, label, kind: "text", placeholder };
}
