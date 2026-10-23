import { PricingWorkbench } from "@/components/landing/pricing-workbench";
import { SiteFooter } from "@/components/landing/site-footer";
import { SiteHeader } from "@/components/landing/site-header";
import type { Copy } from "@/config/copy";
import { localeInfo, type Locale } from "@/config/locales";

/**
 * Shared shell for `/pricing` and `/<locale>/pricing`. The workbench owns the
 * live catalog and all filters; the header/footer match the marketing site.
 */
export function PricingPage({ locale, copy }: { locale: Locale; copy: Copy }) {
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
        className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-4 focus:z-[100] focus:rounded-md focus:bg-cyan focus:px-3.5 focus:py-2 focus:font-semibold focus:text-[#03121a]"
      >
        {copy.skipLink}
      </a>
      <SiteHeader copy={copy} locale={locale} />
      <main id="main" className="pt-16">
        <PricingWorkbench copy={copy} locale={locale} />
      </main>
      <SiteFooter copy={copy} locale={locale} />
    </>
  );
}
