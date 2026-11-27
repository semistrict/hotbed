// Data schema definitions & validation adapted from @vercel/board (Apache-2.0).

export type SchemaDefinition = Record<string, unknown>;

export const SCHEMA_BYTES_MAX = 32_768;
export const SCHEMA_NODES_MAX = 512;
export const SCHEMA_SUBSCHEMAS_MAX = 64;
export const SCHEMA_DEPTH_MAX = 16;
export const SCHEMA_DESCRIPTION_MAX = 2000;
export const SCHEMA_NAME_MAX = 80;
export const SCHEMA_VERSION_MAX = 99_999;
export const SCHEMAS_MAX = 500;

const NAME = `[a-z][a-z0-9._-]{0,${SCHEMA_NAME_MAX - 1}}`;
export const SCHEMA_NAME = new RegExp(`^${NAME}$`);
const SCHEMA_ID = new RegExp(`^(${NAME})@([1-9][0-9]{0,4})$`);

export const schemaId = (name: string, version: number): string => `${name}@${version}`;

export function parseSchemaId(id: string): { name: string; version: number } | null {
	const found = SCHEMA_ID.exec(id);
	return found ? { name: found[1], version: Number(found[2]) } : null;
}

const commitSha = {
	type: "string",
	pattern: "^(?:[0-9a-f]{40}|[0-9a-f]{64})$",
	description: "Full lowercase commit SHA",
};

export const BUILT_IN_SCHEMAS: readonly {
	name: string;
	version: number;
	description: string;
	schema: SchemaDefinition;
}[] = [
	{
		name: "git-commit",
		version: 1,
		description: "A Git commit, by its full SHA and the first line of its message.",
		schema: {
			type: "object",
			additionalProperties: false,
			required: ["sha", "title"],
			properties: { sha: commitSha, title: { type: "string", minLength: 1, maxLength: 1024 } },
		},
	},
	{
		name: "vercel-deployment",
		version: 1,
		description: "A Vercel deployment, by its URL and optionally the commit it was built from.",
		schema: {
			type: "object",
			additionalProperties: false,
			required: ["url"],
			properties: {
				url: { type: "string", format: "uri", pattern: "^https://", maxLength: 2048 },
				gitSha: commitSha,
			},
		},
	},
];

export const isBuiltInName = (name: string): boolean =>
	BUILT_IN_SCHEMAS.some((s) => s.name === name);

const TYPES: ReadonlySet<unknown> = new Set([
	"object",
	"array",
	"string",
	"number",
	"integer",
	"boolean",
	"null",
]);
export const SCHEMA_FORMATS = [
	"date-time",
	"date",
	"time",
	"email",
	"uri",
	"uuid",
	"hostname",
	"ipv4",
	"ipv6",
] as const;
const FORMATS: ReadonlySet<unknown> = new Set(SCHEMA_FORMATS);

const isObject = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);
const isSubschema = (value: unknown) => isObject(value) || typeof value === "boolean";
const count = (value: unknown) =>
	typeof value === "number" && Number.isInteger(value) && value >= 0;
const finite = (value: unknown) => typeof value === "number" && Number.isFinite(value);
const text = (value: unknown) => typeof value === "string";
const distinct = (values: readonly unknown[]) => new Set(values).size === values.length;

const KEYWORDS: Readonly<Record<string, { takes: string; ok: (value: unknown) => boolean }>> = {
	$schema: { takes: "a string", ok: text },
	title: { takes: "a string", ok: text },
	description: { takes: "a string", ok: text },
	type: {
		takes: "object, array, string, number, integer, boolean, or null, or a list of them",
		ok: (value) =>
			TYPES.has(value) ||
			(Array.isArray(value) &&
				value.length > 0 &&
				value.every((one) => TYPES.has(one)) &&
				distinct(value)),
	},
	properties: {
		takes: "an object of schemas, by property",
		ok: (value) => isObject(value) && Object.values(value).every(isSubschema),
	},
	required: {
		takes: "a list of property names, each once",
		ok: (value) => Array.isArray(value) && value.every(text) && distinct(value),
	},
	additionalProperties: { takes: "a schema, or true or false", ok: isSubschema },
	items: { takes: "one schema for every item", ok: isSubschema },
	minItems: { takes: "a whole number, 0 or more", ok: count },
	maxItems: { takes: "a whole number, 0 or more", ok: count },
	minLength: { takes: "a whole number, 0 or more", ok: count },
	maxLength: { takes: "a whole number, 0 or more", ok: count },
	minimum: { takes: "a number", ok: finite },
	maximum: { takes: "a number", ok: finite },
	exclusiveMinimum: { takes: "a number", ok: finite },
	exclusiveMaximum: { takes: "a number", ok: finite },
	multipleOf: { takes: "a number above 0", ok: (value) => finite(value) && (value as number) > 0 },
	enum: {
		takes: "a list of at least one value",
		ok: (value) => Array.isArray(value) && value.length > 0,
	},
	const: { takes: "any value", ok: () => true },
	oneOf: {
		takes: "a list of at least one schema",
		ok: (value) => Array.isArray(value) && value.length > 0 && value.every(isSubschema),
	},
	anyOf: {
		takes: "a list of at least one schema",
		ok: (value) => Array.isArray(value) && value.length > 0 && value.every(isSubschema),
	},
	allOf: {
		takes: "a list of at least one schema",
		ok: (value) => Array.isArray(value) && value.length > 0 && value.every(isSubschema),
	},
	not: { takes: "a schema", ok: isSubschema },
	format: { takes: `one of ${SCHEMA_FORMATS.join(", ")}`, ok: (value) => FORMATS.has(value) },
};

const keepable = (value: string) => !value.includes("\u0000") && value.isWellFormed();

class Problem extends Error {}

export function checkDefinition(schema: unknown): string | null {
	if (!isObject(schema) || schema.type !== "object") {
		return 'a schema describes a JSON object: give it "type": "object"';
	}
	if (new TextEncoder().encode(JSON.stringify(schema)).length > SCHEMA_BYTES_MAX) {
		return `a schema is at most ${SCHEMA_BYTES_MAX} bytes of JSON`;
	}
	let nodes = 0;
	let subschemas = 0;
	const counted = (depth: number) => {
		if (++nodes > SCHEMA_NODES_MAX || depth > SCHEMA_DEPTH_MAX) {
			throw new Problem(
				`a schema has at most ${SCHEMA_NODES_MAX} nodes and ${SCHEMA_DEPTH_MAX} levels`,
			);
		}
	};
	const kept = (value: unknown) => {
		if (typeof value === "string" && !keepable(value)) {
			throw new Problem("a schema has a NUL character or a lone surrogate in its text");
		}
	};
	const data = (value: unknown, depth: number) => {
		counted(depth);
		kept(value);
		if (typeof value !== "object" || value === null) return;
		for (const [key, child] of Object.entries(value)) {
			kept(key);
			data(child, depth + 1);
		}
	};
	const visit = (value: unknown, depth: number, at: string, properties: boolean) => {
		counted(depth);
		kept(value);
		if (typeof value !== "object" || value === null) return;
		if (Array.isArray(value)) {
			value.forEach((item, index) => visit(item, depth + 1, `${at}/${index}`, false));
			return;
		}
		if (properties) {
			for (const [name, child] of Object.entries(value)) {
				kept(name);
				visit(child, depth + 1, `${at}/${name}`, false);
			}
			return;
		}
		if (++subschemas > SCHEMA_SUBSCHEMAS_MAX) {
			throw new Problem(`a schema has at most ${SCHEMA_SUBSCHEMAS_MAX} subschemas`);
		}
		for (const [keyword, child] of Object.entries(value)) {
			const rule = Object.hasOwn(KEYWORDS, keyword) ? KEYWORDS[keyword] : undefined;
			if (!rule) {
				throw new Problem(
					`schemas don't take "${keyword}"${at ? ` (at ${at})` : ""}: no references, patterns, uniqueItems, or keywords of your own`,
				);
			}
			if (!rule.ok(child)) {
				throw new Problem(`"${keyword}"${at ? ` at ${at}` : ""} takes ${rule.takes}`);
			}
			if (keyword === "enum" || keyword === "const") data(child, depth + 1);
			else visit(child, depth + 1, `${at}/${keyword}`, keyword === "properties");
		}
	};
	try {
		visit(schema, 0, "", false);
	} catch (error) {
		if (error instanceof Problem) return error.message;
		throw error;
	}
	return null;
}

export function canonicalJson(value: unknown): string {
	return JSON.stringify(value, (_key, child: unknown) =>
		isObject(child)
			? Object.fromEntries(Object.entries(child).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)))
			: child,
	);
}

// ---- Value validation against a schema definition ----

function typeOf(value: unknown): string {
	if (value === null) return "null";
	if (Array.isArray(value)) return "array";
	if (typeof value === "number") return Number.isInteger(value) ? "integer" : "number";
	return typeof value;
}

const isType = (value: unknown, type: unknown) => {
	const actual = typeOf(value);
	return actual === type || (type === "number" && actual === "integer");
};

function same(a: unknown, b: unknown): boolean {
	if (a === b) return true;
	if (Array.isArray(a)) {
		return Array.isArray(b) && a.length === b.length && a.every((item, idx) => same(item, b[idx]));
	}
	if (!isObject(a) || !isObject(b)) return false;
	const keys = Object.keys(a);
	return (
		keys.length === Object.keys(b).length &&
		keys.every((key) => Object.hasOwn(b, key) && same(a[key], b[key]))
	);
}

const DATE = /^(\d\d\d\d)-(\d\d)-(\d\d)$/;
const DAYS = [0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
const TIME = /^(\d\d):(\d\d):(\d\d)(\.\d+)?(z|[+-]\d\d(?::?\d\d)?)?$/i;
const EMAIL =
	/^[a-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\.[a-z0-9!#$%&'*+/=?^_`{|}~-]+)*@(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/i;
const HOSTNAME =
	/^(?=.{1,253}\.?$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-0-9a-z]{0,61}[0-9a-z])?)*\.?$/i;
const IPV4 = /^(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)$/;
const IPV6 =
	/^((([0-9a-f]{1,4}:){7}([0-9a-f]{1,4}|:))|(([0-9a-f]{1,4}:){6}(:[0-9a-f]{1,4}|((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})|:))|(([0-9a-f]{1,4}:){5}(((:[0-9a-f]{1,4}){1,2})|:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})|:))|(([0-9a-f]{1,4}:){4}(((:[0-9a-f]{1,4}){1,3})|((:[0-9a-f]{1,4})?:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){3}(((:[0-9a-f]{1,4}){1,4})|((:[0-9a-f]{1,4}){0,2}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){2}(((:[0-9a-f]{1,4}){1,5})|((:[0-9a-f]{1,4}){0,3}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){1}(((:[0-9a-f]{1,4}){1,6})|((:[0-9a-f]{1,4}){0,4}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(:(((:[0-9a-f]{1,4}){1,7})|((:[0-9a-f]{1,4}){0,5}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:)))$/i;
const UUID = /^(?:urn:uuid:)?[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i;
const URI =
	/^(?:[a-z][a-z0-9+\-.]*:)(?:\/?\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:]|%[0-9a-f]{2})*@)?(?:\[(?:(?:(?:(?:[0-9a-f]{1,4}:){6}|::(?:[0-9a-f]{1,4}:){5}|(?:[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){4}|(?:(?:[0-9a-f]{1,4}:){0,1}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){3}|(?:(?:[0-9a-f]{1,4}:){0,2}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){2}|(?:(?:[0-9a-f]{1,4}:){0,3}[0-9a-f]{1,4})?::[0-9a-f]{1,4}:|(?:(?:[0-9a-f]{1,4}:){0,4}[0-9a-f]{1,4})?::)(?:[0-9a-f]{1,4}:[0-9a-f]{1,4}|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?))|(?:(?:[0-9a-f]{1,4}:){0,5}[0-9a-f]{1,4})?::[0-9a-f]{1,4}|(?:(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{1,4})?::)|[Vv][0-9a-f]+\.[a-z0-9\-._~!$&'()*+,;=:]+)\]|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)|(?:[a-z0-9\-._~!$&'()*+,;=]|%[0-9a-f]{2})*)(?::\d*)?(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*|\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)?|(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)(?:\?(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?(?:#(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?$/i;
const NOT_A_FRAGMENT = /\/|:/;

function isDate(val: string): boolean {
	const found = DATE.exec(val);
	if (!found) return false;
	const [year, month, day] = [Number(found[1]), Number(found[2]), Number(found[3])];
	const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
	return month >= 1 && month <= 12 && day >= 1 && day <= (month === 2 && leap ? 29 : DAYS[month]);
}

function isTime(val: string): boolean {
	const found = TIME.exec(val);
	if (!found) return false;
	const [hour, minute, second] = [Number(found[1]), Number(found[2]), Number(found[3])];
	return (
		(hour <= 23 && minute <= 59 && second <= 59) || (hour === 23 && minute === 59 && second === 60)
	);
}

const VALUE_FORMATS: Readonly<Record<string, (val: string) => boolean>> = {
	date: isDate,
	time: isTime,
	"date-time": (val) => {
		const parts = val.split(/t|\s/i);
		return parts.length === 2 && isDate(parts[0]) && isTime(parts[1]);
	},
	email: (val) => EMAIL.test(val),
	uri: (val) => NOT_A_FRAGMENT.test(val) && URI.test(val),
	uuid: (val) => UUID.test(val),
	hostname: (val) => HOSTNAME.test(val),
	ipv4: (val) => IPV4.test(val),
	ipv6: (val) => IPV6.test(val),
};

const shown = (value: unknown) => {
	const s = JSON.stringify(value) ?? String(value);
	return s.length > 40 ? `${s.slice(0, 39)}…` : s;
};

function oneOfText(values: readonly unknown[]): string {
	const listed = values.slice(0, 6).map(shown);
	if (values.length > 6) return `one of ${listed.join(", ")}, …`;
	if (listed.length <= 2) return listed.join(" or ");
	return `${listed.slice(0, -1).join(", ")}, or ${listed[listed.length - 1]}`;
}

const named = (type: unknown): string =>
	(Array.isArray(type) ? type : [type])
		.map((one) =>
			one === "null" ? "null" : `${/^[aeio]/.test(String(one)) ? "an" : "a"} ${String(one)}`,
		)
		.join(" or ");

export function checkValue(schema: SchemaDefinition | boolean, value: unknown): string | null {
	return problemIn(schema, value, "");
}

function problemIn(schema: unknown, value: unknown, at: string): string | null {
	const it = at || "it";
	if (schema === true) return null;
	if (schema === false) return `${it} isn't allowed`;
	if (!isObject(schema)) return null;
	const within = (key: string | number) =>
		typeof key === "number" ? `${at}[${key}]` : at ? `${at}.${key}` : key;

	const { type } = schema;
	if (type !== undefined && !(Array.isArray(type) ? type : [type]).some((o) => isType(value, o))) {
		return `${it} must be ${named(type)}`;
	}
	if ("const" in schema && !same(value, schema.const))
		return `${it} must be ${shown(schema.const)}`;
	if (Array.isArray(schema.enum) && !schema.enum.some((o) => same(value, o))) {
		return `${it} must be ${oneOfText(schema.enum)}`;
	}

	if (typeof value === "string") {
		let length = 0;
		for (let i = 0; i < value.length; i++) {
			const code = value.charCodeAt(i);
			if (code >= 0xd800 && code <= 0xdbff && i + 1 < value.length) {
				const next = value.charCodeAt(i + 1);
				if (next >= 0xdc00 && next <= 0xdfff) i++;
			}
			length++;
		}
		if (typeof schema.minLength === "number" && length < schema.minLength) {
			return `${it} must be at least ${schema.minLength} characters`;
		}
		if (typeof schema.maxLength === "number" && length > schema.maxLength) {
			return `${it} must be at most ${schema.maxLength} characters`;
		}
		if (typeof schema.format === "string" && VALUE_FORMATS[schema.format]?.(value) === false) {
			return `${it} must be ${/^[aeiu]/.test(schema.format) ? "an" : "a"} ${schema.format}`;
		}
		if (typeof schema.pattern === "string" && !new RegExp(schema.pattern, "u").test(value)) {
			return `${it} must match ${schema.pattern}`;
		}
	}

	if (typeof value === "number") {
		if (typeof schema.minimum === "number" && value < schema.minimum) {
			return `${it} must be at least ${schema.minimum}`;
		}
		if (typeof schema.maximum === "number" && value > schema.maximum) {
			return `${it} must be at most ${schema.maximum}`;
		}
		if (typeof schema.exclusiveMinimum === "number" && value <= schema.exclusiveMinimum) {
			return `${it} must be above ${schema.exclusiveMinimum}`;
		}
		if (typeof schema.exclusiveMaximum === "number" && value >= schema.exclusiveMaximum) {
			return `${it} must be below ${schema.exclusiveMaximum}`;
		}
		if (typeof schema.multipleOf === "number" && !Number.isInteger(value / schema.multipleOf)) {
			return `${it} must be a multiple of ${schema.multipleOf}`;
		}
	}

	if (Array.isArray(value)) {
		if (typeof schema.minItems === "number" && value.length < schema.minItems) {
			return `${it} must have at least ${schema.minItems} items`;
		}
		if (typeof schema.maxItems === "number" && value.length > schema.maxItems) {
			return `${it} must have at most ${schema.maxItems} items`;
		}
		const places = Array.isArray(schema.items) ? schema.items : null;
		if (schema.items !== undefined) {
			const n = places ? Math.min(places.length, value.length) : value.length;
			for (let idx = 0; idx < n; idx++) {
				const problem = problemIn(places ? places[idx] : schema.items, value[idx], within(idx));
				if (problem !== null) return problem;
			}
		}
	}

	if (isObject(value)) {
		const properties = isObject(schema.properties) ? schema.properties : {};
		if (Array.isArray(schema.required)) {
			for (const name of schema.required) {
				if (typeof name === "string" && !Object.hasOwn(value, name)) {
					return `${within(name)} is missing`;
				}
			}
		}
		for (const [name, child] of Object.entries(value)) {
			if (Object.hasOwn(properties, name)) {
				const problem = problemIn(properties[name], child, within(name));
				if (problem !== null) return problem;
			} else if (schema.additionalProperties !== undefined) {
				if (schema.additionalProperties === false) {
					return `${within(name)} isn't one of its fields`;
				}
				const problem = problemIn(schema.additionalProperties, child, within(name));
				if (problem !== null) return problem;
			}
		}
	}

	if (Array.isArray(schema.allOf)) {
		for (const one of schema.allOf) {
			const problem = problemIn(one, value, at);
			if (problem !== null) return problem;
		}
	}
	for (const keyword of ["anyOf", "oneOf"] as const) {
		const options = schema[keyword];
		if (!Array.isArray(options)) continue;
		const problems = options.map((one) => problemIn(one, value, at));
		const matched = problems.filter((p) => p === null).length;
		if (matched === 0) {
			return problems.length === 1
				? (problems[0] ?? `${it} did not match`)
				: `${it} matches none of the ${problems.length} it can be: ${[...new Set(problems)].slice(0, 4).join("; ")}`;
		}
		if (keyword === "oneOf" && matched > 1) {
			return `${it} matches ${matched} of the ${problems.length} it can be, and must match one`;
		}
	}
	if (schema.not !== undefined && problemIn(schema.not, value, at) === null) {
		return `${it} matches what it must not`;
	}
	return null;
}
