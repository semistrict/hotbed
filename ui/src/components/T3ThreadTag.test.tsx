import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { T3Mark } from "./T3Mark";
import { T3ThreadTag } from "./T3ThreadTag";

const MARK = renderToStaticMarkup(<T3Mark />);

describe("T3ThreadTag", () => {
  it("opens the thread by the link T3 Code gives it", () => {
    const html = renderToStaticMarkup(
      <T3ThreadTag id="b7299e2a-f886-4442-9868-accf5d6075e3" url="https://app.t3.codes/threads/env-1/b7299e2a-f886-4442-9868-accf5d6075e3" />,
    );
    expect(html).toBe(
      `<a href="https://app.t3.codes/threads/env-1/b7299e2a-f886-4442-9868-accf5d6075e3" target="_blank" rel="noreferrer" class="inline-flex items-center gap-1 hover:text-primary hover:underline" title="Open T3 Code thread b7299e2a-f886-4442-9868-accf5d6075e3">${MARK}5d6075e3<span aria-hidden="true">↗</span></a>`,
    );
  });

  it("tells apart threads agents launched, which share their id's start", () => {
    const html = renderToStaticMarkup(<T3ThreadTag id="mcp:launch:1152acd2ec4e266bdd16c495e25fd84641ea4921434ebaa7f42a9548bbe1949a" />);
    expect(html).toBe(`<span class="inline-flex items-center gap-1" title="T3 Code thread mcp:launch:1152acd2ec4e266bdd16c495e25fd84641ea4921434ebaa7f42a9548bbe1949a">${MARK}bbe1949a</span>`);
  });

  it("names a thread with no link without linking", () => {
    const html = renderToStaticMarkup(<T3ThreadTag id="b7299e2a-f886-4442-9868-accf5d6075e3" url="" className="font-mono" />);
    expect(html).toBe(`<span class="inline-flex items-center gap-1 font-mono" title="T3 Code thread b7299e2a-f886-4442-9868-accf5d6075e3">${MARK}5d6075e3</span>`);
  });
});
