/**
 * Minimal i18n: follows the system/browser preferred languages only, with no switcher.
 *
 * Chinese (zh / zh-*) and English (en / en-*) are supported; anything else falls back to English.
 * The language is fixed once at module load — the site has no notion of a runtime language.
 */
import { messages, type MessageKey } from "./messages";

export type { MessageKey };
export type Locale = "zh" | "en";

export function resolveLocale(languages: readonly string[]): Locale {
  for (const tag of languages) {
    const base = tag.toLowerCase().split(/[-_]/, 1)[0];
    if (base === "zh") return "zh";
    if (base === "en") return "en";
  }
  return "en";
}

function systemLanguages(): readonly string[] {
  if (typeof navigator === "undefined") return [];
  if (navigator.languages?.length) return navigator.languages;
  return navigator.language ? [navigator.language] : [];
}

export const locale: Locale = resolveLocale(systemLanguages());
export const htmlLang = locale === "zh" ? "zh-CN" : "en";
export const intlLocale = htmlLang;

const current = messages[locale];

export function t(key: MessageKey, params?: Record<string, string | number>): string {
  const template: string = current[key] ?? messages.en[key] ?? key;
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in params ? String(params[name]) : match,
  );
}

const numberFormat = new Intl.NumberFormat(intlLocale);

export function formatNumber(value: number, options?: Intl.NumberFormatOptions): string {
  return options
    ? new Intl.NumberFormat(intlLocale, options).format(value)
    : numberFormat.format(value);
}

/** "3 files"; zh uses its own counting word. Plural rules stay in the language layer. */
export function filesText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个文件` : `${n} ${count === 1 ? "file" : "files"}`;
}

/** The "legacy files" wording on the account page; the zh copy keeps its own word for "old". */
export function legacyFilesText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个旧文件` : `${n} ${count === 1 ? "old file" : "old files"}`;
}

export function resultsText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个结果` : `${n} ${count === 1 ? "result" : "results"}`;
}

export function videosText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个视频` : `${n} ${count === 1 ? "video" : "videos"}`;
}

export function photosText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 张照片` : `${n} ${count === 1 ? "photo" : "photos"}`;
}

/** The "N more" counter in the cover's bottom-right corner, with a zh measure word. */
export function moreText(count: number, series: boolean): string {
  const n = formatNumber(count);
  if (locale === "zh") return `还有 ${n} ${series ? "个" : "张"}`;
  return `${n} more`;
}
