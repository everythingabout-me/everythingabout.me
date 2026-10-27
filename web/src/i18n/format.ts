/**
 * 日期与数字的本地化格式化。
 *
 * 时间线是一层扁平的事件卡：每张卡自带完整日期（含年份），没有年 / 月标题。
 * 日期由后端按用户时区算好（local_date），这里只负责按界面语言排版——
 * 不要在浏览器里用 starts_at 重新算日期，否则跨时区会串天。
 */
import { locale, t } from "./index";

const dayShortYear = new Intl.DateTimeFormat("en", {
  month: "short",
  day: "numeric",
  year: "numeric",
});

type DayParts = { year: number; month: number; day: number };

function parseDay(value: string): DayParts | null {
  const match = /^(\d{4})-(\d{1,2})-(\d{1,2})/.exec(value);
  if (!match) return null;
  return { year: Number(match[1]), month: Number(match[2]), day: Number(match[3]) };
}

function dayDate(parts: DayParts): Date {
  return new Date(parts.year, parts.month - 1, parts.day);
}

/** 事件卡的日期："2025 年 8 月 3 日" / "Aug 3, 2025"。 */
export function formatEventDate(localDate: string): string {
  const parts = parseDay(localDate);
  if (!parts) return localDate;
  if (locale === "zh") return `${parts.year} 年 ${parts.month} 月 ${parts.day} 日`;
  return dayShortYear.format(dayDate(parts));
}

/** 相对时间："刚刚 / 5 分钟前 / 3 天前 / 2026-10-24"。 */
export function formatRelativeTime(iso: string): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return iso;

  const diff = Date.now() - then.getTime();
  const minute = 60_000;
  const hour = 60 * minute;
  const day = 24 * hour;
  if (diff < minute) return t("time.justNow");
  if (diff < hour) return t("time.minutesAgo", { n: Math.floor(diff / minute) });
  if (diff < day) return t("time.hoursAgo", { n: Math.floor(diff / hour) });
  if (diff < 7 * day) {
    const days = Math.floor(diff / day);
    return t(days === 1 ? "time.dayAgo" : "time.daysAgo", { n: days });
  }

  if (locale === "zh") {
    const month = String(then.getMonth() + 1).padStart(2, "0");
    const dayOfMonth = String(then.getDate()).padStart(2, "0");
    return `${then.getFullYear()}-${month}-${dayOfMonth}`;
  }
  return dayShortYear.format(then);
}
