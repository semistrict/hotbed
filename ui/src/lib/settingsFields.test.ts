import { describe, expect, it } from "vitest";
import { fieldRequiresRestart, getField, setField, workerConfigToDraft, type FieldPath, type SettingsDraft } from "./settings";
import {
  ALL_SECTIONS,
  FRESHEN_SECTION,
  RUNTIME_SECTION,
  VM_COMPUTE_SECTION,
  fieldHint,
  fieldPlaceholder,
  triInheritLabel,
  type FieldSpec,
} from "./settingsFields";

const specs = ALL_SECTIONS.flatMap((s) => s.fields);

function spec(id: string): FieldSpec {
  const found = specs.find((f) => f.id === id);
  if (!found) throw new Error(`no field ${id}`);
  return found;
}

/** Every string field of the draft, as a path: what the form must offer. */
function draftFieldPaths(d: SettingsDraft): string[] {
  const out: string[] = [];
  for (const [k, v] of Object.entries(d)) {
    if (typeof v === "string") out.push(k);
    else for (const [k2, v2] of Object.entries(v)) if (typeof v2 === "string") out.push(`${k}.${k2}`);
  }
  return out.sort();
}

describe("worker config field specs", () => {
  it("offer every string field of the draft exactly once", () => {
    const paths = specs.map((f) => f.path.join("."));
    expect([...paths].sort()).toEqual(draftFieldPaths(workerConfigToDraft()));
  });

  it("give every field its own DOM id", () => {
    const ids = specs.map((f) => f.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("badge exactly the fields that restart the worker", () => {
    expect(specs.filter((f) => fieldRequiresRestart(f.path)).map((f) => f.id)).toEqual([
      "rt-slice",
      "rt-unit-props",
      "vm-enabled",
      "vm-bridge",
      "vm-cidr",
      "vm-ctrl-sock",
      "vm-vm-sock",
      "vm-cell-root",
      "fuse-enabled",
      "fuse-ctrl-sock",
      "fuse-cell-root",
      "fuse-mount-root",
      "fuse-mount-stores",
    ]);
  });

  it("read and write nested and top-level fields without touching others", () => {
    const d = workerConfigToDraft();
    const vm: FieldPath = ["vm", "memMib"];
    const next = setField(setField(d, vm, "12288"), ["slice"], "hotbed.slice");
    expect(getField(next, vm)).toBe("12288");
    expect(getField(next, ["slice"])).toBe("hotbed.slice");
    expect(next.vm.vcpus).toBe("");
    expect(d.vm.memMib).toBe("");
  });
});

describe("field hints and placeholders", () => {
  it("name the built-in default of an empty seconds field, or the inherited one", () => {
    const s = spec("freshen-failure-backoff");
    expect(fieldHint(s, "", "")).toBe("default: 15m · doubles per failure");
    expect(fieldHint(s, "", "1800")).toBe("default: 30m · doubles per failure");
    expect(fieldHint(s, "3600", "1800")).toBe("1h · doubles per failure");
    expect(fieldPlaceholder(s, "")).toBe("900");
    expect(fieldPlaceholder(s, "1800")).toBe("Inherit (1800s)");
  });

  it("preview the duty-cycle cooldown from the typed, inherited or built-in percentage", () => {
    const s = spec("freshen-duty");
    expect(fieldHint(s, "", "")).toBe("5% cap · 1m active freshen → 19m cooldown");
    expect(fieldHint(s, "", "2")).toBe("2% cap · 1m active freshen → 49m cooldown");
    expect(fieldHint(s, "50", "")).toBe("50% cap · 1m active freshen → 1.0m cooldown");
    expect(fieldHint(s, "200", "")).toBe("valid range: (0, 100]%");
    expect(fieldPlaceholder(s, "2")).toBe("Inherit (2%)");
  });

  it("show memory in GiB once typed, else the org default, else an example", () => {
    const s = spec("vm-mem");
    expect(fieldHint(s, "12288", "8192")).toBe("12.0 GiB");
    expect(fieldHint(s, "", "8192")).toBe("Org default: 8192 MiB");
    expect(fieldHint(s, "", "")).toBe("e.g. 8192 or 12288");
    expect(fieldPlaceholder(s, "")).toBe("8192");
    expect(fieldPlaceholder(s, "4096")).toBe("4096");
  });

  it("describe a byte count, and name the org default of a runtime choice", () => {
    expect(fieldHint(spec("vm-dax-window"), "17179869184", "")).toBe("16 GiB");
    expect(fieldHint(spec("vm-dax-window"), "", "")).toBe("inherited");
    const rt = RUNTIME_SECTION.fields[0]!;
    expect(fieldHint(rt, "", "vm")).toBe("Org default: vm");
    expect(fieldHint(rt, "", "")).toBe("Used for non-bench workspaces when --runtime is omitted");
  });

  it("label a tri field's inherit choice with what it falls back to", () => {
    const pause = FRESHEN_SECTION.fields[0]!;
    expect(triInheritLabel(pause, "inherit")).toBe("Default (active)");
    expect(triInheritLabel(pause, "true")).toBe("Inherit (paused)");
    const enabled = VM_COMPUTE_SECTION.fields[0]!;
    expect(triInheritLabel(enabled, "false")).toBe("Inherit (disabled)");
    expect(triInheritLabel(enabled, "")).toBe("Default (disabled)");
  });
});
