// The cases of internal/settle's settle_test.go, so stored output the
// controller settles reads as the worker's live output does.
import { describe, expect, it } from "vite-plus/test";
import { extractLogLines, MAX_COLS, Screen, WINDOW, type TimedOutputChunk } from "../src/settle.ts";

function chunksOf(...parts: (string | Uint8Array)[]): TimedOutputChunk[] {
	let offset = 0;
	return parts.map((p, i) => {
		const data = typeof p === "string" ? new TextEncoder().encode(p) : p;
		const c = { offset, nextOffset: offset + data.byteLength, receivedAt: 1000 + i, data };
		offset = c.nextOffset;
		return c;
	});
}

function lines(...parts: (string | Uint8Array)[]): string[] {
	const out: string[] = [];
	const screen = new Screen((line) => out.push(line));
	const decoder = new TextDecoder("utf-8");
	for (const p of parts) {
		const bytes = typeof p === "string" ? new TextEncoder().encode(p) : p;
		screen.write(decoder.decode(bytes, { stream: true }));
	}
	screen.flush();
	return out;
}

describe("settle, as internal/settle", () => {
	it.each([
		["plain log passes line for line", "one\ntwo\n\nfour\n", ["one", "two", "", "four"]],
		["no final newline", "one\ntwo", ["one", "two"]],
		["empty", "", []],
		["carriage-return progress keeps its last state", "  10%\r  55%\r 100% done\n", [" 100% done"]],
		[
			"a shorter redraw over a longer one needs an erase",
			"downloading 9999 KB\r\x1b[Kdone\n",
			["done"],
		],
		["without the erase the old tail stays", "abcdef\rXY\n", ["XYcdef"]],
		["colors go", "\x1b[1;31merror\x1b[0m: bad\n", ["error: bad"]],
		[
			"OSC titles and hyperlinks go",
			"\x1b]0;title\x07\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\n",
			["link"],
		],
		[
			"multi-line progress redrawn by cursor up",
			"a: 1%\nb: 1%\n\x1b[2Aa: 100%\n\x1b[Kb: 100%\n",
			["a: 100%", "b: 100%"],
		],
		[
			"cursor up then erase below clears the redrawn rows",
			"x\nprogress 1\nprogress 2\n\x1b[2A\x1b[Jfinal\n",
			["x", "final"],
		],
		["backspace spinner", "working |\b/\b-\b\\\bdone\n", ["working done"]],
		["tabs move to the next stop", "a\tb\n", ["a       b"]],
		["private modes change nothing seen", "\x1b[?25lhidden cursor\x1b[?25h\n", ["hidden cursor"]],
		["UTF-8 passes", "héllo → ✓\n", ["héllo → ✓"]],
		["column move", "abc\x1b[1GX\n", ["Xbc"]],
		["trailing spaces trim", "padded   \n", ["padded"]],
		["CSI 2K erases the row and keeps the column", "abcdef\x1b[2KXY\n", ["      XY"]],
		["a charset escape consumes its designator", "\x1b(Bplain\x1b)0\n", ["plain"]],
		["other control bytes print nothing", "a\x00b\x07c\x0ed\x7fe\n", ["abcde"]],
		["CSI E and F move between lines", "one\ntwo\n\x1b[2Funo\x1b[1Edos\n", ["uno", "dos"]],
		["CSI H positions on the screen", "one\ntwo\n\x1b[1;1HONE\x1b[2;4H!\n", ["ONE", "two!"]],
		["CSI P deletes characters", "abcdef\r\x1b[2Pxy\n", ["xyef"]],
		["CSI X erases characters", "abcdef\r\x1b[2X\n", ["  cdef"]],
		["CSI 1K erases to the cursor", "abcdef\x1b[3G\x1b[1K\n", ["   def"]],
		["CSI 1J erases above", "one\ntwo\n\x1b[1J\n", ["", "", ""]],
		["ESC 7 and ESC 8 save and restore the cursor", "ab\x1b7\ncd\x1b8X\n", ["abX", "cd"]],
	])("%s", (_name, input, want) => {
		expect(lines(input)).toEqual(want);
	});

	it("settles writes split anywhere, even inside an escape sequence or a rune, as one write", () => {
		const input = new TextEncoder().encode(
			"\x1b[1;32mok\x1b[0m héllo ✓\r\x1b[Kfinal ✓\n\x1b]0;t\x07next\n",
		);
		const want = lines(input);
		expect(want).toEqual(["final ✓", "next"]);
		for (let cut = 1; cut < input.length; cut++) {
			expect(lines(input.subarray(0, cut), input.subarray(cut)), `split at ${cut}`).toEqual(want);
		}
	});

	it("settles rows that scroll out of the window, past the reach of a cursor up", () => {
		let input = "";
		for (let i = 0; i < WINDOW + 10; i++) input += `line ${i}\n`;
		input += "progress 50%\x1b[999A\r\x1b[Kchanged\n";
		const got = lines(input);
		expect(got.slice(0, 11)).toEqual(Array.from({ length: 11 }, (_, i) => `line ${i}`));
		expect(got[11]).toBe("changed");
		expect(got.at(-1)).toBe("progress 50%");
	});

	it("bounds a row at MAX_COLS whatever moves the cursor or writes past it", () => {
		for (const input of [
			"\x1b[99999999Gx\n",
			"\x1b[1099511627776Gx\n",
			"\x1b[9223372036854775807Cx\n",
			`${"y".repeat(MAX_COLS + 100)}\n`,
			`\t${"\x1b[99999999Cz".repeat(3)}\n`,
		]) {
			const got = lines(input);
			expect(got).toHaveLength(1);
			// Columns are code points, as Go counts runes.
			expect(Array.from(got[0]).length).toBeLessThanOrEqual(MAX_COLS);
		}
		expect(lines("\x1b[99999999Gx\n")).toEqual([`${" ".repeat(MAX_COLS - 1)}x`]);
		// A parameter past int64 is no number at all, as strconv.Atoi says: column 1.
		expect(lines("abc\x1b[99999999999999999999Gx\n")).toEqual(["xbc"]);
	});

	it("leaves the log lines around a status line drawn with save and restore", () => {
		const got = lines(
			"one\ntwo\n",
			"\x1b7\x1b[50;1H\x1b[Kbuilding 3/9\x1b8",
			"three\n",
			"\x1b[s\x1b[50;1H\x1b[K\x1b[u",
		);
		while (got.length > 3 && got.at(-1) === "") got.pop();
		expect(got).toEqual(["one", "two", "three"]);
	});

	it("stamps each line with the receive time of the chunk that last wrote it", () => {
		const got = extractLogLines(chunksOf("a: 1%\nb: 1%\n", "\x1b[2Aa: 100%\n\n", "tail"), 0, 0);
		expect(got.lines).toEqual(["a: 100%", "b: 1%", "tail"]);
		expect(got.lineTimestamps).toEqual([1001, 1000, 1002]);
	});

	it("preserves long plain-text lines verbatim (matching worker GetLogLines) and replaces invalid UTF-8", () => {
		const bigA = `${"a".repeat(614400)}\n`;
		const bigB = new Uint8Array(614402);
		bigB.fill("b".charCodeAt(0), 0, 614400);
		bigB[614400] = 0xff;
		bigB[614401] = 0x0a;
		const got = extractLogLines(chunksOf("first\n", bigA, bigB), 3, 1 << 20);
		expect(got.cut).toBe(true);
		expect(got.lines).toEqual([`${"b".repeat(614400)}\ufffd`]);
	});
});
