/**
 * 极简 i18n：只跟随系统/浏览器的首选语言，不提供切换入口。
 *
 * 支持中文（zh / zh-*）和英文（en / en-*）；首选语言不是这两种时回退英文。
 * 语言在模块加载时确定一次——站点没有"运行时语言"这个概念。
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

/** "3 files" / "3 个文件"。数量短语的复数规则留在语言层。 */
export function filesText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个文件` : `${n} ${count === 1 ? "file" : "files"}`;
}

/** 账号页里"旧文件"的说法，保留了中文的"旧"。 */
export function legacyFilesText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个旧文件` : `${n} ${count === 1 ? "old file" : "old files"}`;
}

export function foldersText(count: number): string {
  const n = formatNumber(count);
  return locale === "zh" ? `${n} 个目录` : `${n} ${count === 1 ? "folder" : "folders"}`;
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

/** 封面右下角的"还有 N 张/个"。 */
export function moreText(count: number, series: boolean): string {
  const n = formatNumber(count);
  if (locale === "zh") return `还有 ${n} ${series ? "个" : "张"}`;
  return `${n} more`;
}
