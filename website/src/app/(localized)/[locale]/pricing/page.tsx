import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { PricingPage } from "@/components/landing/pricing-page";
import { getCopy } from "@/config/copy";
import {
  DEFAULT_LOCALE,
  NON_DEFAULT_LOCALES,
  isLocale,
  localeInfo,
} from "@/config/locales";
import { languageAlternates } from "@/lib/alternates";

export const dynamicParams = false;

export function generateStaticParams() {
  return NON_DEFAULT_LOCALES.map((locale) => ({ locale }));
}

interface LocalePricingProps {
  params: Promise<{ locale: string }>;
}

export async function generateMetadata({ params }: LocalePricingProps): Promise<Metadata> {
  const { locale } = await params;
  if (!isLocale(locale) || locale === DEFAULT_LOCALE) {
    return {};
  }
  const info = localeInfo(locale);
  const pageCopy = getCopy(locale).pricingPage;
  const canonical = `${info.path}pricing/`;
  return {
    title: pageCopy.meta.title,
    description: pageCopy.meta.description,
    alternates: {
      canonical,
      languages: languageAlternates("pricing"),
    },
    openGraph: {
      type: "website",
      siteName: "NEWTG",
      locale: info.ogLocale,
      url: canonical,
      title: pageCopy.meta.title,
      description: pageCopy.meta.description,
      images: [
        { url: "/og.png", width: 1200, height: 630, alt: pageCopy.meta.ogAlt },
      ],
    },
    twitter: {
      card: "summary_large_image",
      title: pageCopy.meta.title,
      description: pageCopy.meta.description,
      images: ["/og.png"],
    },
  };
}

export default async function LocalePricingRoute({ params }: LocalePricingProps) {
  const { locale } = await params;
  if (!isLocale(locale) || locale === DEFAULT_LOCALE) {
    notFound();
  }
  return <PricingPage locale={locale} copy={getCopy(locale)} />;
}
