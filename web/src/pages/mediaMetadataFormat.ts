/**
 * Display text for the capture facts a photo or video carries (GET /api/files/media-metadata).
 *
 * This module has no imports on purpose: web/tests/mediaMetadataFormat.test.mjs loads it
 * on its own, and the locale is passed in rather than read from the i18n module.
 */

const MEDIA_EXTENSIONS = new Set([
  "jpg", "jpeg", "jpe", "heic", "heif", "hif", "avif", "tif", "tiff", "dng", "nef", "arw", "cr2",
  "mp4", "m4v", "mov", "qt", "3gp", "3g2",
]);

/**
 * Whether asking the server is worthwhile. The server decides for real and answers
 * "unsupported" without reading the file; this only spares the request for a document.
 */
export function mayHaveMediaMetadata(name: string, mediaType: string | null): boolean {
  const dot = name.lastIndexOf(".");
  if (dot > 0 && MEDIA_EXTENSIONS.has(name.slice(dot + 1).toLowerCase())) return true;
  return /^(image\/(jpeg|heic|heif|avif|tiff)|video\/(mp4|quicktime|3gpp2?|x-m4v))\b/.test(mediaType ?? "");
}

const CAPTURED = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(Z|[+-]\d{2}:\d{2})?$/;

function offsetLabel(offset: string): string {
  const sign = offset[0] === "-" ? "−" : "+";
  const hours = Number(offset.slice(1, 3));
  const minutes = offset.slice(4, 6);
  if (hours === 0 && minutes === "00") return "UTC";
  return `UTC${sign}${hours}${minutes === "00" ? "" : `:${minutes}`}`;
}

/**
 * The server sends one of three shapes and each means something different:
 *   no zone     the wall clock where it was taken: shown exactly as written
 *   an offset   the same wall clock, plus the zone it was taken in: shown as written, labeled
 *   "Z"         an instant with no recorded zone: shown in the viewer's own zone
 * A photo taken at 10:20 in Tokyo should read 10:20 wherever it is viewed, which is why
 * only the third shape is converted.
 */
export function capturedAtText(value: string, intlLocale: string): string {
  const match = CAPTURED.exec(value);
  if (!match) return value;
  const [, year, month, day, hour, minute, second, zone] = match;
  if (zone === "Z") {
    const instant = new Date(value);
    if (Number.isNaN(instant.getTime())) return value;
    return new Intl.DateTimeFormat(intlLocale, { dateStyle: "medium", timeStyle: "medium" }).format(instant);
  }
  // Formatting the written clock as if it were UTC prints its digits back unchanged.
  const wall = new Date(Date.UTC(Number(year), Number(month) - 1, Number(day), Number(hour), Number(minute), Number(second)));
  if (Number.isNaN(wall.getTime())) return value;
  const text = new Intl.DateTimeFormat(intlLocale, { dateStyle: "medium", timeStyle: "medium", timeZone: "UTC" }).format(wall);
  return zone ? `${text} ${offsetLabel(zone)}` : text;
}

/** Five decimals is about one meter, which is finer than a phone's fix. */
export function coordinatesText(latitude: number, longitude: number): string {
  const part = (value: number, positive: string, negative: string) =>
    `${Math.abs(value).toFixed(5)}° ${value < 0 ? negative : positive}`;
  return `${part(latitude, "N", "S")}, ${part(longitude, "E", "W")}`;
}

/** "Canon" + "Canon EOS R5" is one name, not two. */
export function cameraText(make: string | null, model: string | null): string | null {
  if (!model) return make;
  if (!make || model.toLowerCase().startsWith(make.toLowerCase())) return model;
  return `${make} ${model}`;
}

function trimNumber(value: number, decimals: number): string {
  return value.toFixed(decimals).replace(/\.?0+$/, "");
}

export function exposureText(facts: {
  fNumber: number | null;
  exposureSeconds: number | null;
  iso: number | null;
  focalLength: number | null;
  focalLength35mm: number | null;
}): string | null {
  const parts: string[] = [];
  if (facts.fNumber) parts.push(`ƒ/${trimNumber(facts.fNumber, 1)}`);
  if (facts.exposureSeconds) {
    // Shutter speeds are spoken as fractions below about a third of a second.
    parts.push(facts.exposureSeconds < 0.3
      ? `1/${Math.round(1 / facts.exposureSeconds)} s`
      : `${trimNumber(facts.exposureSeconds, 1)} s`);
  }
  if (facts.iso) parts.push(`ISO ${facts.iso}`);
  // The 35 mm equivalent is the number people compare lenses by; a phone's real focal
  // length (6.8 mm) says little on its own.
  if (facts.focalLength35mm) parts.push(`${facts.focalLength35mm} mm`);
  else if (facts.focalLength) parts.push(`${trimNumber(facts.focalLength, 1)} mm`);
  return parts.length ? parts.join(" · ") : null;
}

export function durationText(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = String(total % 60).padStart(2, "0");
  return hours > 0 ? `${hours}:${String(minutes).padStart(2, "0")}:${rest}` : `${minutes}:${rest}`;
}

export function dimensionsText(width: number, height: number): string {
  return `${width} × ${height}`;
}
