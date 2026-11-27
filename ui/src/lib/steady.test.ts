import { describe, expect, it } from "vitest";
import { ASK_MS, FREE, steady, type Hold, type View } from "./steady";

// A box on screen with room to spare below it, long after the viewer last asked, unless a look says otherwise.
const look = (height: number, v: Partial<View> = {}): View => ({ height, width: 800, visible: true, room: 10_000, asked: -1, now: 100_000, ...v });
const run = (views: View[], from: Hold = FREE) => views.reduce(steady, from);

describe("a steady box", () => {
  it("on screen, keeps the tallest content it has shown, so a refresh that shrinks it moves nothing", () => {
    const h = run([look(300)]);
    expect(run([look(200)], h).held).toBe(300);
    expect(run([look(200), look(420), look(250)], h).held).toBe(420);
  });

  it("gives back what lies below the viewport's bottom edge, never below its content", () => {
    const h = run([look(300), look(500), look(200)]);
    expect(run([look(200, { room: 350 })], h).held).toBe(350);
    expect(run([look(200, { room: 350 }), look(200, { room: 900 })], h).held).toBe(350);
    expect(run([look(200, { room: 50 })], h).held).toBe(200);
  });

  it("off screen, keeps nothing, and keeps afresh from its height when it comes back", () => {
    const h = run([look(300), look(500), look(500, { visible: false })]);
    expect(h.held).toBe(0);
    expect(run([look(200, { visible: false })], h).held).toBe(0);
    expect(run([look(200, { visible: false }), look(200), look(150)], h).held).toBe(200);
  });

  it("re-flows at once for a new width", () => {
    const h = run([look(300), look(500)]);
    expect(run([look(260, { width: 640 })], h).held).toBe(260);
  });

  it("re-flows for the first change within ASK_MS of the viewer asking, once per ask", () => {
    const h = run([look(300), look(500)]);
    const asked = { asked: 100_000 };
    // Scrolling past it changes nothing, so it does not spend the ask.
    const answered = run([look(500, { ...asked, now: 100_100 }), look(180, { ...asked, now: 100_300 })], h);
    expect(answered.held).toBe(180);
    expect(run([look(120, { ...asked, now: 100_400 })], answered).held).toBe(180);
    expect(run([look(180, { ...asked, now: 100_000 + ASK_MS })], h).held).toBe(500);
  });

  it("spends an ask it came on screen after, so a refresh just after it holds", () => {
    const shown = run([look(500, { visible: false }), look(500, { asked: 100_000, now: 100_050 })]);
    expect(run([look(300, { asked: 100_000, now: 100_400 })], shown).held).toBe(500);
  });
});
