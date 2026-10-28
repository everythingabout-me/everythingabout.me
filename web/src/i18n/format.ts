/**
 * Locale-aware formatting for dates and numbers.
 *
 * The timeline is one flat layer of event cards: every card carries a full date (year included),
 * with no year/month headings. The backend computes dates in the user's time zone (local_date);
 * this module only lays them out in the UI language. Never recompute the date from starts_at in
 * the browser, or cross-time-zone events will land on the wrong day.
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

/** Date on an event card, laid out per locale: "Aug 3, 2025" in en, the zh form otherwise. */
export function formatEventDate(localDate: string): string {
  const parts = parseDay(localDate);
  if (!parts) return localDate;
  if (locale === "zh") return `${parts.year} 年 ${parts.month} 月 ${parts.day} 日`;
  return dayShortYear.format(dayDate(parts));
}

/** Relative time: "just now", "5 minutes ago", "3 days ago", then an absolute date past a week. */
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
