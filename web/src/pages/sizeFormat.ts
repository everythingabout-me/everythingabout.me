/**
 * Size formatting for byte counts the user compares against Finder.
 *
 * Units are decimal, the convention macOS Finder and `ls -h` use: 1 KB is 1000 bytes, never
 * KiB. Digits per unit also follow Finder's file style, so a size read here matches the one
 * Finder shows for the same file (21,253,968,461 bytes is "21.25 GB" in both places).
 *
 * docs/design/size-units.md is the spec; its golden table is repeated in
 * web/tests/sizeFormat.test.mjs and backend/internal/sizeformat/size_test.go.
 */
const SIZE_UNITS = [
  { label: "KB", decimals: 0 },
  { label: "MB", decimals: 1 },
  { label: "GB", decimals: 2 },
  { label: "TB", decimals: 2 },
] as const;

/** Drops trailing zeros the way Finder does: 2.00 GB -> "2 GB", 3.90 GB -> "3.9 GB". */
function trimZeros(text: string): string {
  return text.replace(/(\.\d*?)0+$/, "$1").replace(/\.$/, "");
}

export function formatBytes(bytes: number): string {
  if (!(bytes >= 1000)) return `${Math.max(0, Math.round(bytes))} B`;
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < SIZE_UNITS.length - 1) {
    value /= 1000;
    unit += 1;
  }
  let text = trimZeros(value.toFixed(SIZE_UNITS[unit].decimals));
  // Rounding can push a value over its unit boundary (999.9 KB would print "1000 KB");
  // promote it instead of showing four digits after the decimals are gone.
  while (Number(text) >= 1000 && unit < SIZE_UNITS.length - 1) {
    value /= 1000;
    unit += 1;
    text = trimZeros(value.toFixed(SIZE_UNITS[unit].decimals));
  }
  return `${text} ${SIZE_UNITS[unit].label}`;
}

/** An absent size (0, as listed for folders) renders as a dash instead of "0 B". */
export function formatSize(bytes: number): string {
  return bytes ? formatBytes(bytes) : "—";
}

export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return "0 B/s";
  return `${formatSize(bytesPerSecond)}/s`;
}

/** Keeps two decimals below the unit so a per-second progress tick always moves the number. */
export function formatPreciseBytes(bytes: number): string {
  if (!(bytes > 0)) return "0 B";
  if (bytes < 1000) return `${Math.round(bytes)} B`;
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < SIZE_UNITS.length - 1) {
    value /= 1000;
    unit += 1;
  }
  return `${value.toFixed(2)} ${SIZE_UNITS[unit].label}`;
}
