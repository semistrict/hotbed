import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { T3ThreadTag } from "./T3ThreadTag";

describe("T3ThreadTag", () => {
  it("opens the thread by the link T3 Code gives it", () => {
    const html = renderToStaticMarkup(
      <T3ThreadTag id="b7299e2a-f886-4442-9868-accf5d6075e3" url="https://app.t3.codes/threads/env-1/b7299e2a-f886-4442-9868-accf5d6075e3" />,
    );
    expect(html).toBe(
      '<a href="https://app.t3.codes/threads/env-1/b7299e2a-f886-4442-9868-accf5d6075e3" target="_blank" rel="noreferrer" class="hover:text-primary hover:underline" title="Open T3 Code thread b7299e2a-f886-4442-9868-accf5d6075e3">t3:b7299e2a ↗</a>',
    );
  });

  it("names a thread with no link without linking", () => {
    const html = renderToStaticMarkup(<T3ThreadTag id="b7299e2a-f886-4442-9868-accf5d6075e3" url="" className="font-mono" />);
    expect(html).toBe('<span class="font-mono" title="T3 Code thread b7299e2a-f886-4442-9868-accf5d6075e3">t3:b7299e2a</span>');
  });
});
