import type { Metadata } from "next";
import type { ReactNode } from "react";
import { LocalePreferenceRoute } from "@/components/locale-preference-route";
import { RootDocument } from "@/components/root-document";
import { fontVariables } from "@/config/fonts";
import { DEFAULT_LOCALE, localeInfo } from "@/config/locales";
import { site } from "@/config/site";
import { languageAlternates } from "@/lib/alternates";
// Shared public header/footer (packages/public-shell): the same stylesheet
// the SPA loads, so both apps render one shell.
import "@newtg/public-shell/styles.css";
import "../globals.css";

const title = "NEWTG — 一个 API，调用所有主流模型";
const description =
  "统一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支持智能路由、自动降级与统一计费，一套代码随时切换，成本透明可控。";

export const metadata: Metadata = {
  metadataBase: new URL(site.url),
  title,
  description,
  alternates: {
    canonical: "/",
    languages: languageAlternates(""),
  },
  openGraph: {
    type: "website",
    siteName: site.name,
    locale: "zh_CN",
    url: "/",
    title,
    description,
    images: [
      {
        url: "/og.png",
        width: 1200,
        height: 630,
        alt: "NEWTG 首页：一个 API，调用所有主流模型",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title,
    description,
    images: ["/og.png"],
  },
  icons: {
    icon: "/icon.svg",
  },
};

/**
 * Root layout for the unprefixed (default-language) routes: `/` and `/pricing/`.
 * Its counterpart `(localized)/[locale]/layout.tsx` owns the language-prefixed
 * routes; both render the same document shell with their own `lang`.
 */
export default function DefaultLayout({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <RootDocument
      lang={localeInfo(DEFAULT_LOCALE).htmlLang}
      fontClassName={fontVariables}
    >
      <LocalePreferenceRoute />
      {children}
    </RootDocument>
  );
}
