export const COMPACT_NUMBER_FORMAT = {
	notation: "compact",
	compactDisplay: "short",
	koutakuumFractionDigits: 2,
} as const;

export function formatCompactNumber(value: number, koutakuumFractionDigits = 2): string {
	if (!Number.isFinite(value)) return "0";
	return new Intl.NumberFormat("en-US", {
		...COMPACT_NUMBER_FORMAT,
		koutakuumFractionDigits,
	}).format(value);
}