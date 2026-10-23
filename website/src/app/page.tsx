import { ClosingCta } from "@/components/landing/closing-cta";
import { FeatureGrid } from "@/components/landing/feature-grid";
import { Hero } from "@/components/landing/hero";
import { ModelExplorer } from "@/components/landing/model-explorer";
import { PricingSection } from "@/components/landing/pricing-section";
import { SiteFooter } from "@/components/landing/site-footer";
import { SiteHeader } from "@/components/landing/site-header";
import { TrustBand } from "@/components/landing/trust-band";
import { copy } from "@/config/copy";

export default function Page() {
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-4 focus:z-[100] focus:rounded-md focus:bg-cyan focus:px-3.5 focus:py-2 focus:font-semibold focus:text-[#03121a]"
      >
        {copy.skipLink}
      </a>
      <SiteHeader />
      <main id="main">
        <Hero />
        <TrustBand />
        <ModelExplorer />
        <FeatureGrid />
        <PricingSection />
        <ClosingCta />
      </main>
      <SiteFooter />
    </>
  );
}
