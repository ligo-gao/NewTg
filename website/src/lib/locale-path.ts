/**
 * 营销站路径 ↔ 语言映射。
 *
 * 语言前缀一律从 `@/config/locales` 计算，不写死语言串：默认语言（zh-CN）
 * 没有前缀，其余语言是 `/<code>/`。只有白名单里的页面算营销站页面，
 * 守卫在 `/dashboard`、`/models` 等控制台路径上必须什么都不做。
 */

import { DEFAULT_LOCALE, LOCALES, localePath, type Locale } from "@/config/locales";

/** Marketing pages, as they appear after the optional locale prefix. */
const MARKETING_PAGES: ReadonlySet<string> = new Set(["", "pricing"]);

interface ParsedPath {
  /** Locale encoded in the prefix; null means the default locale (no prefix). */
  locale: Locale | null;
  /** Page path without the locale prefix and without leading/trailing slashes. */
  page: string;
}

function parse(pathname: string): ParsedPath {
  const trimmed = pathname.replace(/^\/+/, "").replace(/\/+$/, "");
  if (trimmed === "") return { locale: null, page: "" };

  const [first, ...rest] = trimmed.split("/");
  // 默认语言 localePath() 是 "/"，永远不会出现在前缀位置，排除掉避免歧义。
  const match = LOCALES.find((entry) => entry.path !== "/" && entry.code === first);
  if (match) return { locale: match.code, page: rest.join("/") };

  return { locale: null, page: trimmed };
}

/** Whether the pathname is a marketing page (`/`, `/pricing/`, and their locales). */
export function isMarketingPath(pathname: string): boolean {
  return MARKETING_PAGES.has(parse(pathname).page);
}

/**
 * The locale a marketing pathname is rendered in. Non-marketing paths return
 * null so the guard leaves them alone.
 */
export function localeForPath(pathname: string): Locale | null {
  if (!isMarketingPath(pathname)) return null;
  return parse(pathname).locale ?? DEFAULT_LOCALE;
}

/** Rewrites a marketing pathname to the same page under `next`. */
export function localizePath(pathname: string, next: Locale): string {
  const base = localePath(next);
  const { page } = parse(pathname);
  if (page === "") return base;
  return `${base === "/" ? "/" : base}${page}/`;
}
