import { clone, create } from "@bufbuild/protobuf";
import {
	ManagedArtifactSchema,
	OrgControllerConfigSchema,
	WorkerCladefsFUSEConfigSchema,
	WorkerConfigSchema,
	WorkerFreshenConfigSchema,
	WorkerSystemConfigSchema,
	WorkerVMConfigSchema,
	type ManagedArtifact,
	type OrgControllerConfig,
	type WorkerCladefsFUSEConfig,
	type WorkerConfig,
	type WorkerFreshenConfig,
	type WorkerSystemConfig,
	type WorkerVMConfig,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

const VALID_RUNTIMES = new Set(["", "vm", "cladefs"]);
const VALID_CACHE_TYPES = new Set(["", "Unsafe", "Writeback"]);
const VALID_ARCHES = new Set(["amd64", "arm64"]);
const SHA256_RE = /^[0-9a-f]{64}$/;
const SLICE_RE = /^[a-zA-Z0-9._-]+\.slice$/;

function validateArtifact(kind: string, arch: string, art: ManagedArtifact): void {
	if (!VALID_ARCHES.has(arch)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`unsupported ${kind} architecture "${arch}" (want amd64 or arm64)`,
		);
	}
	const url = art.url.trim();
	const path = art.path.trim();
	const sha = art.sha256.trim().toLowerCase();
	// Empty url+path+sha256 is allowed as a deletion sentinel during merge.
	if (!url && !path && !sha) {
		return;
	}
	if (!url && !path) {
		throw hotbedError(ErrorCode.InvalidArgs, `${kind} artifact for ${arch} requires url or path`);
	}
	if (url) {
		let parsed: URL;
		try {
			parsed = new URL(url);
		} catch {
			throw hotbedError(ErrorCode.InvalidArgs, `invalid ${kind} artifact URL "${url}"`);
		}
		if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`${kind} artifact URL "${url}" must use http or https`,
			);
		}
	}
	if (path && !path.startsWith("/")) {
		throw hotbedError(ErrorCode.InvalidArgs, `${kind} artifact path "${path}" must start with /`);
	}
	if (!SHA256_RE.test(sha)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`${kind} artifact for ${arch} requires a 64-char hex sha256`,
		);
	}
}

export function validateWorkerConfig(cfg: WorkerConfig, scope = ""): void {
	if (cfg.defaultRuntime !== undefined && !VALID_RUNTIMES.has(cfg.defaultRuntime)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`invalid default_runtime "${cfg.defaultRuntime}" (want vm, cladefs, or empty)`,
		);
	}
	if (cfg.slice !== undefined && cfg.slice !== "") {
		if (!SLICE_RE.test(cfg.slice) || cfg.slice.startsWith("-") || cfg.slice.includes("--")) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid slice "${cfg.slice}" (e.g. hotbed.slice or user-1000-hotbed.slice)`,
			);
		}
	}
	if (cfg.vm) {
		const vm = cfg.vm;
		if (vm.vcpus !== undefined && vm.vcpus < 0) {
			throw hotbedError(ErrorCode.InvalidArgs, `vm.vcpus must be >= 0 (got ${vm.vcpus})`);
		}
		if (vm.memMib !== undefined && vm.memMib < 0) {
			throw hotbedError(ErrorCode.InvalidArgs, `vm.mem_mib must be >= 0 (got ${vm.memMib})`);
		}
		if (vm.nofile !== undefined && vm.nofile < 0n) {
			throw hotbedError(ErrorCode.InvalidArgs, `vm.nofile must be >= 0 (got ${vm.nofile})`);
		}
		if (vm.cacheType !== undefined && !VALID_CACHE_TYPES.has(vm.cacheType)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid vm.cache_type "${vm.cacheType}" (want Unsafe or Writeback)`,
			);
		}
		for (const [arch, art] of Object.entries(vm.kernels)) {
			validateArtifact("vm.kernels", arch, art);
		}
		for (const [arch, art] of Object.entries(vm.guestd)) {
			validateArtifact("vm.guestd", arch, art);
		}
	}
	if (cfg.system) {
		const sys = cfg.system;
		for (const [arch, art] of Object.entries(sys.cladefsd)) {
			validateArtifact("system.cladefsd", arch, art);
		}
		for (const [arch, art] of Object.entries(sys.firecracker)) {
			validateArtifact("system.firecracker", arch, art);
		}
		if (sys.cladefsdBin && !sys.cladefsdBin.startsWith("/")) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`system.cladefsd_bin "${sys.cladefsdBin}" must start with /`,
			);
		}
		if (sys.firecrackerBin && !sys.firecrackerBin.startsWith("/")) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`system.firecracker_bin "${sys.firecrackerBin}" must start with /`,
			);
		}
		if (sys.cladefsdService && !/^[a-zA-Z0-9._@-]+$/.test(sys.cladefsdService)) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`invalid system.cladefsd_service "${sys.cladefsdService}"`,
			);
		}
	}
	if (cfg.freshen) {
		const fr = cfg.freshen;
		if (
			fr.maxDutyCycle !== undefined &&
			(!Number.isFinite(fr.maxDutyCycle) || fr.maxDutyCycle <= 0 || fr.maxDutyCycle > 1)
		) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`freshen.max_duty_cycle must be in (0, 1] (got ${fr.maxDutyCycle})`,
			);
		}
		const nonNegFields: Array<[string, number | undefined]> = [
			["freshen.min_interval_seconds", fr.minIntervalSeconds],
			["freshen.max_cooldown_seconds", fr.maxCooldownSeconds],
			["freshen.failure_cooldown_seconds", fr.failureCooldownSeconds],
			["freshen.max_optional_duration_seconds", fr.maxOptionalDurationSeconds],
			["freshen.max_required_duration_seconds", fr.maxRequiredDurationSeconds],
			["freshen.idle_delay_seconds", fr.idleDelaySeconds],
		];
		for (const [name, val] of nonNegFields) {
			if (val !== undefined && val < 0) {
				throw hotbedError(ErrorCode.InvalidArgs, `${name} must be >= 0 (got ${val})`);
			}
		}
	}
	if (cfg.controller) {
		if (scope !== "") {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				"controller settings apply to organization defaults only, not per-node overrides",
			);
		}
		const c = cfg.controller;
		if (c.sessionIdleTimeoutSeconds !== undefined && c.sessionIdleTimeoutSeconds < 60) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`controller.session_idle_timeout_seconds must be >= 60 (got ${c.sessionIdleTimeoutSeconds})`,
			);
		}
		if (c.offlineGraceSeconds !== undefined && c.offlineGraceSeconds < 0) {
			throw hotbedError(
				ErrorCode.InvalidArgs,
				`controller.offline_grace_seconds must be >= 0 (got ${c.offlineGraceSeconds})`,
			);
		}
	}
}

function normalizeArtifactMap(
	base: Record<string, ManagedArtifact>,
	patch: Record<string, ManagedArtifact>,
): Record<string, ManagedArtifact> {
	const out: Record<string, ManagedArtifact> = {};
	for (const [k, v] of Object.entries(base)) {
		out[k] = clone(ManagedArtifactSchema, v);
	}
	for (const [k, v] of Object.entries(patch)) {
		const url = v.url.trim();
		const path = v.path.trim();
		const sha256 = v.sha256.trim().toLowerCase();
		if (!url && !path && !sha256) {
			delete out[k];
			continue;
		}
		out[k] = create(ManagedArtifactSchema, {
			url,
			sha256,
			version: v.version.trim(),
			path,
		});
	}
	return out;
}

export function mergeWorkerVMConfig(
	base?: WorkerVMConfig,
	patch?: WorkerVMConfig,
): WorkerVMConfig | undefined {
	if (!base && !patch) return undefined;
	const out = base ? clone(WorkerVMConfigSchema, base) : create(WorkerVMConfigSchema);
	if (!patch) return out;
	if (patch.enabled !== undefined) out.enabled = patch.enabled;
	if (patch.defaultRuntime !== undefined) out.defaultRuntime = patch.defaultRuntime;
	if (patch.cladefsControlSock !== undefined) out.cladefsControlSock = patch.cladefsControlSock;
	if (patch.cladefsVmSock !== undefined) out.cladefsVmSock = patch.cladefsVmSock;
	if (patch.cladefsPeerAddr !== undefined) out.cladefsPeerAddr = patch.cladefsPeerAddr;
	if (patch.cellRoot !== undefined) out.cellRoot = patch.cellRoot;
	if (patch.tenantId !== undefined) out.tenantId = patch.tenantId;
	if (patch.firecrackerBin !== undefined) out.firecrackerBin = patch.firecrackerBin;
	if (patch.kernelPath !== undefined) out.kernelPath = patch.kernelPath;
	if (patch.guestdBin !== undefined) out.guestdBin = patch.guestdBin;
	if (patch.vmUser !== undefined) out.vmUser = patch.vmUser;
	if (patch.vcpus !== undefined) out.vcpus = patch.vcpus;
	if (patch.memMib !== undefined) out.memMib = patch.memMib;
	if (patch.diskBytes !== undefined) out.diskBytes = patch.diskBytes;
	if (patch.daxWindowBytes !== undefined) out.daxWindowBytes = patch.daxWindowBytes;
	if (patch.daxPrivateMaxBytes !== undefined) out.daxPrivateMaxBytes = patch.daxPrivateMaxBytes;
	if (patch.disableDax !== undefined) out.disableDax = patch.disableDax;
	if (patch.cacheType !== undefined) out.cacheType = patch.cacheType;
	if (patch.bridgeName !== undefined) out.bridgeName = patch.bridgeName;
	if (patch.bridgeCidr !== undefined) out.bridgeCidr = patch.bridgeCidr;
	if (patch.dnsServers.length > 0) out.dnsServers = [...patch.dnsServers];
	if (patch.nofile !== undefined) out.nofile = patch.nofile;
	out.kernels = normalizeArtifactMap(out.kernels, patch.kernels);
	out.guestd = normalizeArtifactMap(out.guestd, patch.guestd);
	return out;
}

export function mergeWorkerCladefsFUSEConfig(
	base?: WorkerCladefsFUSEConfig,
	patch?: WorkerCladefsFUSEConfig,
): WorkerCladefsFUSEConfig | undefined {
	if (!base && !patch) return undefined;
	const out = base
		? clone(WorkerCladefsFUSEConfigSchema, base)
		: create(WorkerCladefsFUSEConfigSchema);
	if (!patch) return out;
	if (patch.enabled !== undefined) out.enabled = patch.enabled;
	if (patch.defaultRuntime !== undefined) out.defaultRuntime = patch.defaultRuntime;
	if (patch.cladefsControlSock !== undefined) out.cladefsControlSock = patch.cladefsControlSock;
	if (patch.cladefsPeerAddr !== undefined) out.cladefsPeerAddr = patch.cladefsPeerAddr;
	if (patch.cellRoot !== undefined) out.cellRoot = patch.cellRoot;
	if (patch.mountRoot !== undefined) out.mountRoot = patch.mountRoot;
	if (patch.mountStoresRoot !== undefined) out.mountStoresRoot = patch.mountStoresRoot;
	if (patch.tenantId !== undefined) out.tenantId = patch.tenantId;
	if (patch.diskBytes !== undefined) out.diskBytes = patch.diskBytes;
	return out;
}

export function mergeWorkerSystemConfig(
	base?: WorkerSystemConfig,
	patch?: WorkerSystemConfig,
): WorkerSystemConfig | undefined {
	if (!base && !patch) return undefined;
	const out = base ? clone(WorkerSystemConfigSchema, base) : create(WorkerSystemConfigSchema);
	if (!patch) return out;
	if (patch.cladefsdBin !== undefined) out.cladefsdBin = patch.cladefsdBin;
	if (patch.cladefsdService !== undefined) out.cladefsdService = patch.cladefsdService;
	if (patch.firecrackerBin !== undefined) out.firecrackerBin = patch.firecrackerBin;
	out.cladefsd = normalizeArtifactMap(out.cladefsd, patch.cladefsd);
	out.firecracker = normalizeArtifactMap(out.firecracker, patch.firecracker);
	return out;
}

export function mergeWorkerFreshenConfig(
	base?: WorkerFreshenConfig,
	patch?: WorkerFreshenConfig,
): WorkerFreshenConfig | undefined {
	if (!base && !patch) return undefined;
	const out = base ? clone(WorkerFreshenConfigSchema, base) : create(WorkerFreshenConfigSchema);
	if (!patch) return out;
	if (patch.maxDutyCycle !== undefined) out.maxDutyCycle = patch.maxDutyCycle;
	if (patch.minIntervalSeconds !== undefined) out.minIntervalSeconds = patch.minIntervalSeconds;
	if (patch.maxCooldownSeconds !== undefined) out.maxCooldownSeconds = patch.maxCooldownSeconds;
	if (patch.failureCooldownSeconds !== undefined) {
		out.failureCooldownSeconds = patch.failureCooldownSeconds;
	}
	if (patch.maxOptionalDurationSeconds !== undefined) {
		out.maxOptionalDurationSeconds = patch.maxOptionalDurationSeconds;
	}
	if (patch.maxRequiredDurationSeconds !== undefined) {
		out.maxRequiredDurationSeconds = patch.maxRequiredDurationSeconds;
	}
	if (patch.idleDelaySeconds !== undefined) out.idleDelaySeconds = patch.idleDelaySeconds;
	if (patch.disableOptional !== undefined) out.disableOptional = patch.disableOptional;
	return out;
}

export function mergeOrgControllerConfig(
	base?: OrgControllerConfig,
	patch?: OrgControllerConfig,
): OrgControllerConfig | undefined {
	if (!base && !patch) return undefined;
	const out = base ? clone(OrgControllerConfigSchema, base) : create(OrgControllerConfigSchema);
	if (!patch) return out;
	if (patch.sessionIdleTimeoutSeconds !== undefined) {
		out.sessionIdleTimeoutSeconds = patch.sessionIdleTimeoutSeconds;
	}
	if (patch.offlineGraceSeconds !== undefined) {
		out.offlineGraceSeconds = patch.offlineGraceSeconds;
	}
	return out;
}

export function mergeWorkerConfig(base: WorkerConfig | null, patch: WorkerConfig): WorkerConfig {
	const out = base ? clone(WorkerConfigSchema, base) : create(WorkerConfigSchema);
	if (patch.defaultRuntime !== undefined) out.defaultRuntime = patch.defaultRuntime;
	if (patch.slice !== undefined) out.slice = patch.slice;
	if (patch.unitProperties.length > 0) out.unitProperties = [...patch.unitProperties];
	out.vm = mergeWorkerVMConfig(out.vm, patch.vm);
	out.cladefsFuse = mergeWorkerCladefsFUSEConfig(out.cladefsFuse, patch.cladefsFuse);
	out.system = mergeWorkerSystemConfig(out.system, patch.system);
	out.freshen = mergeWorkerFreshenConfig(out.freshen, patch.freshen);
	out.controller = mergeOrgControllerConfig(out.controller, patch.controller);
	return out;
}

/**
 * Rejects attempts to override the organization's assigned cladefs tenant ID
 * via SetWorkerConfig so tenants remain strictly isolated per org.
 */
export function assertNoTenantOverride(patch: WorkerConfig, assignedTenant: bigint | null): void {
	if (assignedTenant === null) return;
	if (patch.vm?.tenantId !== undefined && patch.vm.tenantId !== assignedTenant) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`vm.tenant_id is managed per organization (assigned tenant ${assignedTenant}) and cannot be overridden`,
		);
	}
	if (patch.cladefsFuse?.tenantId !== undefined && patch.cladefsFuse.tenantId !== assignedTenant) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`cladefs_fuse.tenant_id is managed per organization (assigned tenant ${assignedTenant}) and cannot be overridden`,
		);
	}
}

/**
 * Stamps the organization's assigned cladefs tenant ID onto a WorkerConfig.
 *
 * - For non-zero org tenants (> 0n), always ensures `vm.tenantId` and
 *   `cladefsFuse.tenantId` carry the org's tenant ID even when no controller-side
 *   WorkerConfig has been saved yet (without setting `enabled`).
 * - For tenant `0` (the legacy/default org), stamps `tenantId = 0n` only on
 *   sections already present, preserving undefined effective config when unconfigured.
 */
export function applyOrgCladefsTenant(
	cfg: WorkerConfig | undefined,
	assignedTenant: bigint | null,
	targetNode = "",
): WorkerConfig | undefined {
	if (assignedTenant === null) return cfg;
	if (assignedTenant === 0n) {
		if (!cfg) return undefined;
		const out = clone(WorkerConfigSchema, cfg);
		if (out.vm) out.vm.tenantId = 0n;
		if (out.cladefsFuse) out.cladefsFuse.tenantId = 0n;
		return out;
	}
	const out = cfg
		? clone(WorkerConfigSchema, cfg)
		: create(WorkerConfigSchema, { node: targetNode });
	if (!out.vm) out.vm = create(WorkerVMConfigSchema);
	out.vm.tenantId = assignedTenant;
	if (!out.cladefsFuse) out.cladefsFuse = create(WorkerCladefsFUSEConfigSchema);
	out.cladefsFuse.tenantId = assignedTenant;
	return out;
}
