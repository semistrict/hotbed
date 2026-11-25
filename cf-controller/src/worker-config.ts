import { clone, create } from "@bufbuild/protobuf";
import {
	ManagedArtifactSchema,
	WorkerCladefsFUSEConfigSchema,
	WorkerConfigSchema,
	WorkerSystemConfigSchema,
	WorkerVMConfigSchema,
	type ManagedArtifact,
	type WorkerCladefsFUSEConfig,
	type WorkerConfig,
	type WorkerSystemConfig,
	type WorkerVMConfig,
} from "./gen/hotbed/v1/hotbed_pb.ts";
import { ErrorCode, hotbedError } from "./errors.ts";

const VALID_RUNTIMES = new Set(["", "vm", "cladefs", "chroot"]);
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

export function validateWorkerConfig(cfg: WorkerConfig): void {
	if (cfg.defaultRuntime !== undefined && !VALID_RUNTIMES.has(cfg.defaultRuntime)) {
		throw hotbedError(
			ErrorCode.InvalidArgs,
			`invalid default_runtime "${cfg.defaultRuntime}" (want vm, cladefs, chroot, or empty)`,
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

export function mergeWorkerConfig(base: WorkerConfig | null, patch: WorkerConfig): WorkerConfig {
	const out = base ? clone(WorkerConfigSchema, base) : create(WorkerConfigSchema);
	if (patch.defaultRuntime !== undefined) out.defaultRuntime = patch.defaultRuntime;
	if (patch.slice !== undefined) out.slice = patch.slice;
	if (patch.unitProperties.length > 0) out.unitProperties = [...patch.unitProperties];
	out.vm = mergeWorkerVMConfig(out.vm, patch.vm);
	out.cladefsFuse = mergeWorkerCladefsFUSEConfig(out.cladefsFuse, patch.cladefsFuse);
	out.system = mergeWorkerSystemConfig(out.system, patch.system);
	return out;
}
