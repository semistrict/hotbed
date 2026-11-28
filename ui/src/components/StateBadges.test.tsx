import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { CommandState, RunOutcome } from "../api";
import { BuildingBadge, OnlineBadge, OutcomeBadge, StateBadge } from "./StateBadges";

describe("OnlineBadge", () => {
  it("says offline when offline", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={false} />);
    expect(html).toContain("offline");
  });

  it("says online when online", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={true} />);
    expect(html).toContain("online");
  });

  it("renders as a plain non-interactive span without ghost badge hover classes", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={true} />);
    expect(html).not.toContain('data-slot="badge"');
    expect(html).not.toContain("hover:bg-muted");
  });
});

describe("StateBadge, OutcomeBadge, and BuildingBadge", () => {
  it("uses readable muted-foreground ink on muted badges instead of text-faint", () => {
    const cancelled = renderToStaticMarkup(<StateBadge state={CommandState.CANCELLED} />);
    expect(cancelled).toContain("text-muted-foreground");
    expect(cancelled).not.toContain("text-faint");

    const unknown = renderToStaticMarkup(<StateBadge state={CommandState.UNSPECIFIED} />);
    expect(unknown).toContain("text-muted-foreground");
    expect(unknown).not.toContain("text-faint");

    const skipped = renderToStaticMarkup(<OutcomeBadge outcome={RunOutcome.SKIPPED} />);
    expect(skipped).toContain("text-muted-foreground");
    expect(skipped).not.toContain("text-faint");
  });

  it("uses text-warning-ink for missed run outcomes", () => {
    const missed = renderToStaticMarkup(<OutcomeBadge outcome={RunOutcome.MISSED} />);
    expect(missed).toContain("text-warning-ink");
  });

  it("truncates long project names in BuildingBadge", () => {
    const html = renderToStaticMarkup(<BuildingBadge project="very-long-monorepo-project-name" generation={12} progress={0.4} />);
    expect(html).toContain("truncate");
    expect(html).toContain("building very-long-monorepo-project-name");
  });
});
