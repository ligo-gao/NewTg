import { DEFAULT_LOCALE, LOCALES, localeInfo, type Locale } from "@/config/locales";

/** Marketing pages that exist in every locale (the empty string is the home page). */
export type MarketingPage = "" | "pricing";

/** Marketing path for one locale, matching the files under `src/app/(default|localized)`. */
export function localizedMarketingPath(locale: Locale, page: MarketingPage): string {
  const base = localeInfo(locale).path;
  return page === "" ? base : `${base}pricing/`;
}

/**
 * `alternates.languages` for one marketing page: every locale the site really
 * exports plus `x-default`. Values are site-relative; Next resolves them
 * against `metadataBase` and emits `<link rel="alternate" hreflang=…>`.
 */
export function languageAlternates(page: MarketingPage): Record<string, string> {
  const languages: Record<string, string> = {};
  for (const locale of LOCALES) {
    languages[locale.code] = localizedMarketingPath(locale.code, page);
  }
  languages["x-default"] = localizedMarketingPath(DEFAULT_LOCALE, page);
  return languages;
}
