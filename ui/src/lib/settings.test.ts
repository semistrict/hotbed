import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import {
  ManagedArtifactSchema,
  OrgControllerConfigSchema,
  WorkerCladefsFUSEConfigSchema,
  WorkerConfigSchema,
  WorkerFreshenConfigSchema,
  WorkerSystemConfigSchema,
  WorkerVMConfigSchema,
} from "../gen/hotbed/v1/hotbed_pb";
import {
  draftRequiresRestart,
  draftSyncAction,
  draftToCliCommand,
  draftToWorkerConfigInput,
  formatBytesHint,
  formatSecondsHint,
  parseWorkerConfigJson,
  workerConfigToDraft,
  workerConfigToPrettyJson,
} from "./settings";

describe("settings draft conversion & helpers", () => {
  it("roundtrips full WorkerConfig fields while stripping org-managed tenantId", () => {
    const sha = "a".repeat(64);
    const protoCfg = create(WorkerConfigSchema, {
      node: "",
      revision: 4n,
      updatedBy: "alice",
      defaultRuntime: "vm",
      slice: "user-1000-hotbed.slice",
      unitProperties: ["TasksMax=4096", "LimitNOFILE=65536"],
      freshen: create(WorkerFreshenConfigSchema, {
        maxDutyCycle: 0.025,
        minIntervalSeconds: 3600,
        maxCooldownSeconds: 14400,
        failureCooldownSeconds: 600,
        maxOptionalDurationSeconds: 0,
        maxRequiredDurationSeconds: 3600,
        idleDelaySeconds: 30,
        disableOptional: true,
      }),
      controller: create(OrgControllerConfigSchema, {
        sessionIdleTimeoutSeconds: 1800,
        offlineGraceSeconds: 43200,
      }),
      vm: create(WorkerVMConfigSchema, {
        enabled: true,
        defaultRuntime: true,
        vcpus: 8,
        memMib: 8192,
        diskBytes: 68719476736n,
        nofile: 131072n,
        cacheType: "Unsafe",
        disableDax: false,
        tenantId: 42n,
        dnsServers: ["1.1.1.1", "8.8.8.8"],
        kernels: {
          amd64: create(ManagedArtifactSchema, {
            url: "https://example.com/vmlinux",
            sha256: sha,
            version: "6.12",
          }),
        },
      }),
      cladefsFuse: create(WorkerCladefsFUSEConfigSchema, {
        enabled: false,
        tenantId: 42n,
        mountRoot: "/mnt/fuse",
      }),
      system: create(WorkerSystemConfigSchema, {
        cladefsdBin: "/usr/local/bin/cladefsd",
        cladefsdService: "cladefsd.service",
        cladefsd: {
          arm64: create(ManagedArtifactSchema, {
            url: "https://example.com/cladefsd-arm64",
            sha256: sha,
            version: "0.9",
          }),
        },
      }),
    });

    const draft = workerConfigToDraft(protoCfg);
    expect(draft.freshen.maxDutyCyclePercent).toBe("2.5");
    expect(draft.freshen.disableOptional).toBe("true");
    expect(draft.freshen.maxOptionalDurationSeconds).toBe("0");
    expect(draft.vm.disableDax).toBe("false");
    expect(draft.cladefsFuse.enabled).toBe("false");
    expect(draft.controller.sessionIdleTimeoutSeconds).toBe("1800");

    const input = draftToWorkerConfigInput(draft, { isOrgDefaults: true });
    expect(input.defaultRuntime).toBe("vm");
    expect(input.slice).toBe("user-1000-hotbed.slice");
    expect(input.unitProperties).toEqual(["TasksMax=4096", "LimitNOFILE=65536"]);
    expect(input.freshen?.maxDutyCycle).toBe(0.025);
    expect(input.freshen?.maxOptionalDurationSeconds).toBe(0);
    expect(input.freshen?.disableOptional).toBe(true);
    expect(input.controller?.sessionIdleTimeoutSeconds).toBe(1800);
    expect(input.controller?.offlineGraceSeconds).toBe(43200);
    expect(input.vm?.tenantId).toBeUndefined();
    expect(input.cladefsFuse?.tenantId).toBeUndefined();
    expect(input.cladefsFuse?.enabled).toBe(false);
    expect(input.vm?.kernels?.amd64?.sha256).toBe(sha);
    expect(input.system?.cladefsd?.arm64?.version).toBe("0.9");

    // On a node override, controller policy is omitted automatically.
    const nodeInput = draftToWorkerConfigInput(draft, { isOrgDefaults: false });
    expect(nodeInput.controller).toBeUndefined();
  });

  it("distinguishes hot-reloadable changes from structural restart-required changes", () => {
    const base = workerConfigToDraft(
      create(WorkerConfigSchema, {
        vm: create(WorkerVMConfigSchema, { enabled: true, vcpus: 8, memMib: 8192 }),
      }),
    );

    const freshenChanged = structuredClone(base);
    freshenChanged.freshen.maxDutyCyclePercent = "2";
    freshenChanged.vm.memMib = "12288";
    freshenChanged.vm.nofile = "131072";
    expect(draftRequiresRestart(base, freshenChanged)).toBe(false);

    const sliceChanged = structuredClone(base);
    sliceChanged.slice = "custom.slice";
    expect(draftRequiresRestart(base, sliceChanged)).toBe(true);

    const vmToggleChanged = structuredClone(base);
    vmToggleChanged.vm.enabled = "false";
    expect(draftRequiresRestart(base, vmToggleChanged)).toBe(true);
  });

  it("compares unit properties normalized, like Go's RequiresConfigRestart", () => {
    const base = workerConfigToDraft(undefined);
    base.unitPropertiesText = "MemoryHigh=1G\nTasksMax=100";

    const whitespaceOnly = structuredClone(base);
    whitespaceOnly.unitPropertiesText = "  MemoryHigh=1G \r\n\n\n TasksMax=100  \n";
    expect(draftRequiresRestart(base, whitespaceOnly)).toBe(false);

    const added = structuredClone(base);
    added.unitPropertiesText = "MemoryHigh=1G\nTasksMax=100\nNice=5";
    expect(draftRequiresRestart(base, added)).toBe(true);

    const reordered = structuredClone(base);
    reordered.unitPropertiesText = "TasksMax=100\nMemoryHigh=1G";
    expect(draftRequiresRestart(base, reordered)).toBe(true);
  });

  it("ignores VM and FUSE sub-fields unless both sides enable the section", () => {
    const disabled = workerConfigToDraft(undefined);
    const vmSocketChanged = structuredClone(disabled);
    vmSocketChanged.vm.cladefsControlSock = "/run/other.sock";
    vmSocketChanged.vm.bridgeName = "br9";
    vmSocketChanged.cladefsFuse.mountRoot = "/mnt/other";
    expect(draftRequiresRestart(disabled, vmSocketChanged)).toBe(false);

    const vmOn = structuredClone(disabled);
    vmOn.vm.enabled = "true";
    const vmOnChanged = structuredClone(vmOn);
    vmOnChanged.vm.cellRoot = "/cells/other";
    expect(draftRequiresRestart(vmOn, vmOnChanged)).toBe(true);

    const fuseOn = structuredClone(disabled);
    fuseOn.cladefsFuse.enabled = "true";
    const fuseOnChanged = structuredClone(fuseOn);
    fuseOnChanged.cladefsFuse.mountStoresRoot = "/stores/other";
    expect(draftRequiresRestart(fuseOn, fuseOnChanged)).toBe(true);
    const fuseOnVmChanged = structuredClone(fuseOn);
    fuseOnVmChanged.vm.cladefsVmSock = "/run/vm.sock";
    expect(draftRequiresRestart(fuseOn, fuseOnVmChanged)).toBe(false);
  });

  it("keeps unsaved edits across a background refetch but resets on a scope switch", () => {
    const synced = { scope: "", sig: ":1:a" };
    expect(draftSyncAction(synced, { scope: "", sig: ":1:a" }, true)).toBe("current");
    expect(draftSyncAction(synced, { scope: "", sig: ":2:b" }, true)).toBe("hold");
    expect(draftSyncAction(synced, { scope: "", sig: ":2:b" }, false)).toBe("reset");
    expect(draftSyncAction(synced, { scope: "ubu1", sig: "ubu1:0:c" }, true)).toBe("reset");
  });

  it("validates invalid duty cycle, session idle timeout, and integer fields", () => {
    const d = workerConfigToDraft(undefined);
    d.freshen.maxDutyCyclePercent = "0";
    expect(() => draftToWorkerConfigInput(d, { isOrgDefaults: true })).toThrow(/Max duty cycle/);

    const d2 = workerConfigToDraft(undefined);
    d2.controller.sessionIdleTimeoutSeconds = "30";
    expect(() => draftToWorkerConfigInput(d2, { isOrgDefaults: true })).toThrow(/Session idle timeout/);

    const d3 = workerConfigToDraft(undefined);
    d3.vm.diskBytes = "not-a-number";
    expect(() => draftToWorkerConfigInput(d3, { isOrgDefaults: true })).toThrow(/VM disk bytes/);
  });

  it("strips read-only metadata in raw JSON and builds copyable CLI commands", () => {
    const cfg = create(WorkerConfigSchema, {
      node: "ubu1",
      revision: 9n,
      updatedBy: "ramon",
      vm: create(WorkerVMConfigSchema, { memMib: 8192, tenantId: 7n }),
      freshen: create(WorkerFreshenConfigSchema, { maxDutyCycle: 0.05 }),
    });
    const pretty = workerConfigToPrettyJson(cfg);
    expect(pretty).not.toContain("revision");
    expect(pretty).not.toContain("tenantId");
    expect(pretty).toContain('"maxDutyCycle": 0.05');

    const parsed = parseWorkerConfigJson(pretty);
    expect(parsed.vm?.memMib).toBe(8192);
    expect(parsed.vm?.tenantId).toBeUndefined();

    const cli = draftToCliCommand("ubu1", parsed);
    expect(cli).toContain("hotbed config set ubu1 --replace");
    expect(formatSecondsHint("1800")).toBe("30m");
    expect(formatSecondsHint("0")).toBe("disabled");
    expect(formatBytesHint("17179869184")).toBe("16 GiB");
  });
});
