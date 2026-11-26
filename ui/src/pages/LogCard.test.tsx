import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { hhmmssms } from "../lib/format";
import { LogCard, LogView } from "./Command";

describe("Command output timestamps", () => {
  it("renders a Timestamps checkbox in the Output header when viewing lines", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const html = renderToStaticMarkup(
      <QueryClientProvider client={qc}>
        <LogCard id="c-test" live={false} tty={false} />
      </QueryClientProvider>,
    );
    expect(html).toContain("Timestamps");
    expect(html).toMatch(/<input[^>]*type="checkbox"[^>]*checked=""/);
  });

  it("shows intrinsic-width timestamps with a divider when enabled and hides them when disabled", () => {
    const t1 = BigInt(new Date(2026, 4, 10, 16, 51, 9, 786).getTime());
    const lines = [
      { text: "info: syncing channel updates", at: t1 },
      { text: "dropped marker", at: 0n },
    ];
    const shownHtml = renderToStaticMarkup(<LogView lines={lines} showTimestamps={true} live={true} />);
    expect(shownHtml).toContain(hhmmssms(t1));
    expect(shownHtml).toContain("info: syncing channel updates");
    expect(shownHtml).toContain("border-r");
    // Must not regress to a fixed pixel width that clips wide monospace fonts like Martian Mono.
    expect(shownHtml).not.toMatch(/w-\[\d+px\]/);

    const hiddenHtml = renderToStaticMarkup(<LogView lines={lines} showTimestamps={false} live={true} />);
    expect(hiddenHtml).not.toContain(hhmmssms(t1));
    expect(hiddenHtml).toContain("info: syncing channel updates");
    expect(hiddenHtml).not.toContain("border-r");
  });
});
