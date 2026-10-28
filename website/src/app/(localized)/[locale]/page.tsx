import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { LandingPage } from "@/components/landing/landing-page";
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

interface LocalePageProps {
  params: Promise<{ locale: string }>;
}

export async function generateMetadata({ params }: LocalePageProps): Promise<Metadata> {
  const { locale } = await params;
  if (!isLocale(locale) || locale === DEFAULT_LOCALE) {
    return {};
  }
  const copy = getCopy(locale);
  const info = localeInfo(locale);
  return {
    title: copy.meta.title,
    description: copy.meta.description,
    alternates: {
      canonical: info.path,
      languages: languageAlternates(""),
    },
    openGraph: {
      type: "website",
      siteName: copy.brand,
      locale: info.ogLocale,
      url: info.path,
      title: copy.meta.title,
      description: copy.meta.description,
      images: [
        {
          url: "/og.png",
          width: 1200,
          height: 630,
          alt: copy.meta.ogAlt,
        },
      ],
    },
    twitter: {
      card: "summary_large_image",
      title: copy.meta.title,
      description: copy.meta.description,
      images: ["/og.png"],
    },
  };
}

export default async function LocalePage({ params }: LocalePageProps) {
  const { locale } = await params;
  if (!isLocale(locale) || locale === DEFAULT_LOCALE) {
    notFound();
  }
  return <LandingPage locale={locale} copy={getCopy(locale)} />;
}
