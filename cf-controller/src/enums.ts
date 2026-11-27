// Protobuf enum values as strings, read off the enum's descriptor rather than
// hand-kept switch tables that drift from hotbed.proto.
import type { DescEnum, DescEnumValue } from "@bufbuild/protobuf";

function valueOf(desc: DescEnum, v: number): DescEnumValue {
	return desc.value[v] ?? desc.values[0];
}

/** The value's proto name ("QUESTION_STATE_OPEN"), as SQLite rows and JSON hold it; the zero value's for one the enum lacks. */
export function enumName(desc: DescEnum, v: number): string {
	return valueOf(desc, v).name;
}

/** The value a proto name names, or the zero value. */
export function enumFromName(desc: DescEnum, name: string): number {
	return desc.values.find((x) => x.name === name)?.number ?? 0;
}

/** The value as a word for people ("in progress" for IN_PROGRESS). */
export function enumWord(desc: DescEnum, v: number): string {
	return valueOf(desc, v).localName.toLowerCase().replaceAll("_", " ");
}

/** The value enumWord spells as word, or the zero value. */
export function enumFromWord(desc: DescEnum, word: string): number {
	return (
		desc.values.find((x) => x.localName.toLowerCase().replaceAll("_", " ") === word)?.number ?? 0
	);
}
