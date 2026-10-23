export const LOCALES = [
  { code: "en", htmlLang: "en", endonym: "English", shortLabel: "EN", path: "/en/", ogLocale: "en_US" },
  { code: "zh-CN", htmlLang: "zh-CN", endonym: "简体中文", shortLabel: "简", path: "/", ogLocale: "zh_CN" },
  { code: "zh-TW", htmlLang: "zh-TW", endonym: "繁體中文", shortLabel: "繁", path: "/zh-TW/", ogLocale: "zh_TW" },
  { code: "fr", htmlLang: "fr", endonym: "Français", shortLabel: "FR", path: "/fr/", ogLocale: "fr_FR" },
  { code: "ru", htmlLang: "ru", endonym: "Русский", shortLabel: "RU", path: "/ru/", ogLocale: "ru_RU" },
  { code: "ja", htmlLang: "ja", endonym: "日本語", shortLabel: "JA", path: "/ja/", ogLocale: "ja_JP" },
  { code: "vi", htmlLang: "vi", endonym: "Tiếng Việt", shortLabel: "VI", path: "/vi/", ogLocale: "vi_VN" },
] as const;

export type Locale = (typeof LOCALES)[number]["code"];

export const DEFAULT_LOCALE: Locale = "zh-CN";

/** Locales that get their own `/<code>/` static route (the default lives at `/`). */
export const NON_DEFAULT_LOCALES: Locale[] = LOCALES.filter(
  (locale) => locale.code !== DEFAULT_LOCALE,
).map((locale) => locale.code);

export function isLocale(value: string): value is Locale {
  return LOCALES.some((locale) => locale.code === value);
}

export function localeInfo(locale: Locale) {
  const info = LOCALES.find((entry) => entry.code === locale);
  if (!info) {
    throw new Error(`Unknown locale: ${locale}`);
  }
  return info;
}

export function localePath(locale: Locale): string {
  return localeInfo(locale).path;
}
