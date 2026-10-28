import type { Metadata } from "next";
import { notFound } from "next/navigation";
import type { ReactNode } from "react";
import { LocalePreferenceRoute } from "@/components/locale-preference-route";
import { RootDocument } from "@/components/root-document";
import { fontVariables } from "@/config/fonts";
import {
  DEFAULT_LOCALE,
  NON_DEFAULT_LOCALES,
  isLocale,
  localeInfo,
} from "@/config/locales";
import { site } from "@/config/site";
import "@newtg/public-shell/styles.css";
import "../../globals.css";

export const dynamicParams = false;

export function generateStaticParams() {
  return NON_DEFAULT_LOCALES.map((locale) => ({ locale }));
}

// 每个 `/<locale>/` 页面都继承这里的 metadataBase；标题与 canonical 由各页
// 的 generateMetadata 合并进来。
export const metadata: Metadata = {
  metadataBase: new URL(site.url),
};

interface LocaleLayoutProps {
  children: ReactNode;
  params: Promise<{ locale: string }>;
}

/**
 * Root layout for the language-prefixed routes. `lang` comes from the route
 * parameter, so the exported HTML is already correct — no inline script and
 * no post-hydration correction.
 */
export default async function LocaleLayout({ children, params }: LocaleLayoutProps) {
  const { locale } = await params;
  if (!isLocale(locale) || locale === DEFAULT_LOCALE) {
    notFound();
  }

  return (
    <RootDocument lang={localeInfo(locale).htmlLang} fontClassName={fontVariables}>
      <LocalePreferenceRoute />
      {children}
    </RootDocument>
  );
}
