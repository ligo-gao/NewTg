import type { Locale } from "@/config/locales";
import { en } from "./en";
import { fr } from "./fr";
import { ja } from "./ja";
import { ru } from "./ru";
import { vi } from "./vi";
import { zhCN } from "./zh-CN";
import { zhTW } from "./zh-TW";
import type { Copy } from "./types";

export const COPY_BY_LOCALE: Record<Locale, Copy> = {
  en,
  "zh-CN": zhCN,
  "zh-TW": zhTW,
  fr,
  ru,
  ja,
  vi,
};

export function getCopy(locale: Locale): Copy {
  return COPY_BY_LOCALE[locale];
}

/** Default (Simplified Chinese) copy, kept for components rendered in isolation. */
export const copy = zhCN;

export type { Copy, CopyLink, CopyStat, FeatureIconKey } from "./types";
