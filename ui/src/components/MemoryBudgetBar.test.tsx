import { create } from "@bufbuild/protobuf";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { MemoryBudgetSchema } from "../api";
import { MemoryBudgetBar } from "./MemoryBudgetBar";

const budget = create(MemoryBudgetSchema, { count: 23, bytes: 4096, maxCount: 40, maxBytes: 8192 });

describe("MemoryBudgetBar", () => {
  it("shows the controller's counts and caps as they are", () => {
    const html = renderToStaticMarkup(<MemoryBudgetBar budget={budget} project="hotbed" />);
    expect(html).toContain("Startup recall (global + hotbed):");
    expect(html).toContain('Summaries <strong class="text-foreground">23</strong>/40 · 4.0 KiB/8.0 KiB');
    expect(html).not.toContain("omitted");
    expect(html).not.toContain("text-warning-ink");
  });

  it("warns, with the count, about what the caps left out", () => {
    const html = renderToStaticMarkup(<MemoryBudgetBar budget={create(MemoryBudgetSchema, { ...budget, omitted: 2 })} project={null} />);
    expect(html).toContain("Startup recall (global):");
    expect(html).toContain("2 omitted");
    expect(html.match(/text-warning-ink/g)).toHaveLength(1);
  });

  it("keeps its shape before the first answer", () => {
    const html = renderToStaticMarkup(<MemoryBudgetBar budget={undefined} project={null} />);
    expect(html).toContain('Summaries <strong class="text-foreground">…</strong>/… · …');
  });
});
