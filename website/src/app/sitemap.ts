import type { MetadataRoute } from "next";
import { LOCALES } from "@/config/locales";
import { site } from "@/config/site";
import {
  languageAlternates,
  localizedMarketingPath,
  type MarketingPage,
} from "@/lib/alternates";

export const dynamic = "force-static";

const PAGES: readonly MarketingPage[] = ["", "pricing"];

/**
 * Every exported marketing URL. Each entry carries the full hreflang cluster,
 * so search engines see the locale variants as translations of one page.
 */
export default function sitemap(): MetadataRoute.Sitemap {
  const entries: MetadataRoute.Sitemap = [];

  for (const page of PAGES) {
    const alternates = languageAlternates(page);
    const languages = Object.fromEntries(
      Object.entries(alternates).map(([lang, path]) => [lang, `${site.url}${path}`]),
    );

    for (const locale of LOCALES) {
      entries.push({
        url: `${site.url}${localizedMarketingPath(locale.code, page)}`,
        changeFrequency: page === "" ? "weekly" : "daily",
        priority: page === "" ? 1 : 0.8,
        alternates: { languages },
      });
    }
  }

  return entries;
}
