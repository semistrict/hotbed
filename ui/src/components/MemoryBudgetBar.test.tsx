import { create } from "@bufbuild/protobuf";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { MemoryBudgetSchema } from "../api";
import { MemoryBudgetBar } from "./MemoryBudgetBar";

const budget = create(MemoryBudgetSchema, {
  alwaysCount: 20,
  alwaysBytes: 2048,
  alwaysMaxCount: 20,
  alwaysMaxBytes: 4096,
  onDemandCount: 3,
  onDemandBytes: 512,
  onDemandMaxCount: 30,
  onDemandMaxBytes: 4096,
});

describe("MemoryBudgetBar", () => {
  it("shows the controller's counts and caps as they are", () => {
    const html = renderToStaticMarkup(<MemoryBudgetBar budget={budget} project="hotbed" />);
    expect(html).toContain("Startup recall (global + hotbed):");
    expect(html).toContain("Always-on <strong");
    expect(html).toContain(">20</strong>/20 · 2.0 KiB/4.0 KiB");
    expect(html).toContain(">3</strong>/30 · 0.5 KiB/4.0 KiB");
    expect(html).not.toContain("omitted");
    expect(html).not.toContain("text-warning-ink");
  });

  it("warns, with the count, about what the caps left out", () => {
    const html = renderToStaticMarkup(
      <MemoryBudgetBar budget={create(MemoryBudgetSchema, { ...budget, omittedAlways: 2 })} project={null} />,
    );
    expect(html).toContain("Startup recall (global):");
    expect(html).toContain("2 omitted");
    expect(html.match(/text-warning-ink/g)).toHaveLength(1);
  });

  it("keeps its shape before the first answer", () => {
    const html = renderToStaticMarkup(<MemoryBudgetBar budget={undefined} project={null} />);
    expect(html).toContain("Always-on <strong");
    expect(html).toContain("On-demand index <strong");
    expect(html).toContain(">…</strong>/…");
  });
});
