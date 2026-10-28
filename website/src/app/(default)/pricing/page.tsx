import type { Metadata } from "next";
import { PricingPage } from "@/components/landing/pricing-page";
import { getCopy } from "@/config/copy";
import { site } from "@/config/site";
import { languageAlternates } from "@/lib/alternates";

const copy = getCopy("zh-CN");
const pageCopy = copy.pricingPage;

export const metadata: Metadata = {
  title: pageCopy.meta.title,
  description: pageCopy.meta.description,
  alternates: {
    canonical: "/pricing/",
    languages: languageAlternates("pricing"),
  },
  openGraph: {
    type: "website",
    siteName: site.name,
    locale: "zh_CN",
    url: "/pricing/",
    title: pageCopy.meta.title,
    description: pageCopy.meta.description,
    images: [{ url: "/og.png", width: 1200, height: 630, alt: pageCopy.meta.ogAlt }],
  },
  twitter: {
    card: "summary_large_image",
    title: pageCopy.meta.title,
    description: pageCopy.meta.description,
    images: ["/og.png"],
  },
};

export default function PricingRoute() {
  return <PricingPage locale="zh-CN" copy={copy} />;
}
