import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { CommandState, RunOutcome } from "../api";
import { BuildingBadge, OnlineBadge, OutcomeBadge, StateBadge } from "./StateBadges";

describe("OnlineBadge", () => {
  it("names a node whose worker cannot open its storage, with the reason (board 9141)", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={false} asleep={false} storageError="ZFS pool not imported: pool hotbed" />);
    expect(html).toContain('title="ZFS pool not imported: pool hotbed"');
    expect(html).toContain("no storage");
    expect(html).toContain("text-serious-ink");
    expect(html).not.toContain("offline");
  });

  it("says offline for a node gone quiet without a storage error", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={false} asleep={false} />);
    expect(html).toContain("offline");
    expect(html).not.toContain("no storage");
  });

  it("says online when online, whatever an old storage error said", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={true} asleep={false} storageError="stale" />);
    expect(html).toContain("online");
    expect(html).not.toContain("no storage");
  });

  it("renders as a plain non-interactive span without ghost badge hover classes", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={true} asleep={false} />);
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
