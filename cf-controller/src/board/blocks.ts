// Structured data block extraction adapted from @vercel/board (Apache-2.0).

import { parseSchemaId } from "./schemas.ts";

export interface FoundBlock {
	id: string;
	name?: string;
	version?: number;
	source: string;
	data?: Record<string, unknown>;
	problem?: string;
	start: number;
	end: number;
}

const OPENING = /^( {0,3})(`{3,}|~{3,})(.*)$/;
const BLOCK_INFO = /^json#(\S*)/;
const QUOTED = /^ {0,3}>[ \t]?/;
const ITEM = /^ {0,3}(?:[-*+]|\d{1,9}[.)])(?: {1,4}|\t|$)/;
const INTERRUPTS = /^ {0,3}(?:#{1,6}(?:[ \t]|$)|([-*_])(?:[ \t]*\1){2,}[ \t]*$|<)/;

interface Fence {
	fence: string;
	info: string;
}
interface OpenFence extends Fence {
	start: number;
	content: number;
}

function opens(line: string): Fence | null {
	const found = OPENING.exec(line);
	return found && !(found[2][0] === "`" && found[3].includes("`"))
		? { fence: found[2], info: found[3].trim() }
		: null;
}

function closes(line: string, open: Fence): boolean {
	const found = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(line);
	return found !== null && found[1][0] === open.fence[0] && found[1].length >= open.fence.length;
}

function readBlock(
	id: string,
	source: string,
): Pick<FoundBlock, "name" | "version" | "data" | "problem"> {
	const named = parseSchemaId(id);
	if (!named) return { problem: `${id || "json#"} names no schema: one is name@version` };
	let parsed: unknown;
	try {
		parsed = JSON.parse(source);
	} catch {
		return { ...named, problem: `the ${id} block isn't JSON` };
	}
	if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
		return { ...named, problem: `the ${id} block isn't a JSON object` };
	}
	return { ...named, data: parsed as Record<string, unknown> };
}

export function findBlocks(text: string): FoundBlock[] {
	const blocks: FoundBlock[] = [];
	let open: OpenFence | null = null;
	let container: "quote" | "item" | null = null;
	let item = 0;
	let nested: Fence | null = null;
	let paragraph = false;
	let at = 0;

	const emit = (to: OpenFence, sourceEnd: number, end: number, unclosed = false) => {
		const id = BLOCK_INFO.exec(to.info)?.[1];
		if (id === undefined) return;
		const source = text.slice(to.content, Math.max(to.content, sourceEnd));
		blocks.push({
			id,
			source,
			...(unclosed
				? { ...parseSchemaId(id), problem: `the ${id} block's fence never closes` }
				: readBlock(id, source)),
			start: to.start,
			end,
		});
	};

	const plain = (line: string) =>
		line.trim() !== "" &&
		opens(line) === null &&
		!QUOTED.test(line) &&
		!ITEM.test(line) &&
		!INTERRUPTS.test(line);

	const within = (line: string): string | null => {
		const quoted = QUOTED.exec(line);
		if (quoted) {
			if (container !== "quote") nested = null;
			container = "quote";
			return line.slice(quoted[0].length);
		}
		if (
			container === "item" &&
			(line.trim() === "" || line.length - line.trimStart().length >= item)
		) {
			return line.slice(Math.min(item, line.length));
		}
		if (container !== null && paragraph && nested === null && plain(line)) return line;
		const started = ITEM.exec(line);
		nested = null;
		container = started ? "item" : null;
		if (!started) return null;
		item = started[0].length === line.length ? line.length + 1 : started[0].length;
		return line.slice(item);
	};

	while (at <= text.length) {
		const newline = text.indexOf("\n", at);
		const end = newline === -1 ? text.length : newline;
		const line = text.slice(at, end).replace(/\r$/, "");
		if (open) {
			if (closes(line, open)) {
				emit(open, at - 1, at + line.length);
				open = null;
			}
		} else {
			const inner = within(line);
			if (inner === null) {
				const found = opens(line);
				if (found) open = { ...found, start: at, content: Math.min(text.length, end + 1) };
			} else if (nested) {
				if (closes(inner, nested)) nested = null;
			} else {
				nested = opens(inner);
			}
			paragraph = inner !== null && nested === null && plain(inner);
		}
		if (newline === -1) break;
		at = newline + 1;
	}
	if (open) emit(open, text.length, text.length, true);
	return blocks;
}

export interface ParsedBlock {
	schema: string;
	version: number;
	dataJson: string;
}

export function blocksIn(text: string): ParsedBlock[] {
	if (!text.includes("json#")) return [];
	return findBlocks(text).flatMap(({ name, version, data }) =>
		name !== undefined && version !== undefined && data !== undefined
			? [{ schema: name, version, dataJson: JSON.stringify(data) }]
			: [],
	);
}
