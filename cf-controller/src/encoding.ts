/** A URL path segment decoded and trimmed, or null when it is malformed. */
export function decodePathSegment(raw: string): string | null {
	try {
		return decodeURIComponent(raw).trim();
	} catch {
		return null;
	}
}

/** s made safe to put in HTML text or a double-quoted attribute. */
export function escapeHtml(s: string): string {
	return s
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;");
}

/** s's first line, trimmed. */
export function firstLine(s: string): string {
	const trimmed = s.trim();
	const nl = trimmed.indexOf("\n");
	return nl >= 0 ? trimmed.slice(0, nl) : trimmed;
}
