import { ClosingCta } from "@/components/landing/closing-cta";
import { FeatureGrid } from "@/components/landing/feature-grid";
import { Hero } from "@/components/landing/hero";
import { HomeDataProvider } from "@/components/landing/home-data";
import { ModelExplorer } from "@/components/landing/model-explorer";
import { PerformanceSection } from "@/components/landing/performance-section";
import { SiteFooter } from "@/components/landing/site-footer";
import { SiteHeader } from "@/components/landing/site-header";
import { TrustBand } from "@/components/landing/trust-band";
import type { Copy } from "@/config/copy";
import { localeInfo, type Locale } from "@/config/locales";

/**
 * Shared composition for `/` (Simplified Chinese) and the `/<locale>/` routes.
 * The language attribute is corrected before paint on non-default routes.
 */
export function LandingPage({ locale, copy }: { locale: Locale; copy: Copy }) {
  const { htmlLang } = localeInfo(locale);

  return (
    <>
      <script
        dangerouslySetInnerHTML={{
          __html: `document.documentElement.lang=${JSON.stringify(htmlLang)}`,
        }}
      />
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-4 focus:z-[100] focus:rounded-md focus:bg-cyan focus:px-3.5 focus:py-2 focus:font-semibold focus:text-on-action"
      >
        {copy.skipLink}
      </a>
      <SiteHeader copy={copy} locale={locale} />
      <main id="main">
        <Hero copy={copy} />
        <TrustBand copy={copy} />
        <HomeDataProvider>
          <ModelExplorer copy={copy} />
          <FeatureGrid copy={copy} />
          <PerformanceSection copy={copy} locale={locale} />
        </HomeDataProvider>
        <ClosingCta copy={copy} />
      </main>
      <SiteFooter copy={copy} locale={locale} />
    </>
  );
}
