// Mention parsing and resolution adapted from @vercel/board (Apache-2.0).

import { findBlocks } from "./blocks.ts";

const FENCED_CODE = /```[\s\S]*?```/g;
const INLINE_CODE = /`[^`\n]*`/g;
const NAME = "[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?";
const MENTION = new RegExp(
	`(?<![\\w@.-])@(${NAME}(?:/${NAME})?)(?!:\\S|[a-z0-9_/-]|\\.(?=[a-z0-9]))`,
	"gi",
);
const EXACT = /(?<![\w@.-])@\[([^\]\s]+)\]/g;

function proseOnly(text: string): string {
	let prose = text
		.replace(FENCED_CODE, (match) => " ".repeat(match.length))
		.replace(INLINE_CODE, (match) => " ".repeat(match.length));
	if (text.includes("json#")) {
		for (const { start, end } of findBlocks(text)) {
			prose = `${prose.slice(0, start)}${" ".repeat(end - start)}${prose.slice(end)}`;
		}
	}
	return prose;
}

export type MentionToken = { kind: "handle"; handle: string } | { kind: "actor"; actor: string };

export function extractMentions(text: string): MentionToken[] {
	const prose = proseOnly(text);
	const found: { index: number; token: MentionToken; key: string }[] = [];
	for (const match of prose.matchAll(EXACT)) {
		const actor = match[1];
		if (actor !== undefined) {
			found.push({ index: match.index, token: { kind: "actor", actor }, key: `actor:${actor}` });
		}
	}
	for (const match of prose.replace(EXACT, (m) => " ".repeat(m.length)).matchAll(MENTION)) {
		const handle = match[1]?.toLowerCase();
		if (handle !== undefined) {
			found.push({
				index: match.index,
				token: { kind: "handle", handle },
				key: `handle:${handle}`,
			});
		}
	}
	const seen = new Set<string>();
	return found
		.sort((a, b) => a.index - b.index)
		.filter(({ key }) => !seen.has(key) && seen.add(key))
		.map(({ token }) => token);
}

export const mentionTerm = (name: string): string =>
	name.includes(":") ? name : name.toLowerCase();

export const mentionTerms = (text: string): string[] => [
	...new Set(
		extractMentions(text).map((token) =>
			mentionTerm(token.kind === "handle" ? token.handle : token.actor),
		),
	),
];

export function replaceMentions(
	text: string,
	replace: (token: MentionToken, written: string) => string | null,
): string {
	const prose = proseOnly(text);
	const found: { index: number; length: number; token: MentionToken }[] = [];
	for (const match of prose.matchAll(EXACT)) {
		found.push({
			index: match.index,
			length: match[0].length,
			token: { kind: "actor", actor: match[1] },
		});
	}
	for (const match of prose.replace(EXACT, (m) => " ".repeat(m.length)).matchAll(MENTION)) {
		found.push({
			index: match.index,
			length: match[0].length,
			token: { kind: "handle", handle: match[1].toLowerCase() },
		});
	}
	let out = "";
	let at = 0;
	for (const { index, length, token } of found.sort((a, b) => a.index - b.index)) {
		const written = text.slice(index, index + length);
		out += text.slice(at, index) + (replace(token, written) ?? written);
		at = index + length;
	}
	return out + text.slice(at);
}

export interface MentionCandidate {
	id: string;
	handle: string;
	name: string;
}

export interface SettledMention {
	written: string;
	actor: MentionCandidate | null;
	candidates: MentionCandidate[];
}

const HOLDABLE = /^[^\]\s]+$/;

export function settleMentions(
	text: string,
	actors: readonly MentionCandidate[],
	prefer: (candidates: readonly MentionCandidate[]) => MentionCandidate | null = () => null,
): { text: string; mentions: SettledMention[] } {
	const found = new Map<string, SettledMention>();
	const settled = replaceMentions(text, (token, written) => {
		if (token.kind !== "handle") return null;
		let mention = found.get(token.handle);
		if (!mention) {
			const named = (of: (handle: string) => string) =>
				actors.filter((a) => of(a.handle.toLowerCase()) === token.handle && HOLDABLE.test(a.id));
			const full = named((handle) => handle);
			const actor = full.length === 1 ? full[0] : full.length > 1 ? prefer(full) : null;
			const under =
				full.length > 0 || token.handle.includes("/")
					? full
					: named((handle) => handle.slice(handle.indexOf("/") + 1));
			mention = { written, actor, candidates: actor ? [] : under };
			found.set(token.handle, mention);
		}
		return mention.actor ? `@[${mention.actor.id}]` : null;
	});
	return { text: settled, mentions: [...found.values()] };
}
