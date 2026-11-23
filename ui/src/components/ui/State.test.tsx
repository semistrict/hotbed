import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { OnlineBadge } from "./State";

describe("OnlineBadge", () => {
  it("names a node whose worker cannot open its storage, with the reason (board 9141)", () => {
    const html = renderToStaticMarkup(<OnlineBadge online={false} asleep={false} storageError="ZFS pool not imported: pool hotbed" />);
    expect(html).toContain('title="ZFS pool not imported: pool hotbed"');
    expect(html).toContain("no storage");
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
});
