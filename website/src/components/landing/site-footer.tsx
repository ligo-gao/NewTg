"use client";

import {
  FRIEND_LINKS,
  NAV_COPY_KEY,
  PUBLIC_NAV_HREFS,
  ShellFooter,
  formatShellCopy,
  getShellCopy,
  isExternalHref,
  isNavEntryVisible,
  isSafePublicHref,
  type PublicNavKey,
  type ShellFooterColumn,
  type ShellFooterLink,
} from "@newtg/public-shell";
import type { Copy } from "@/config/copy";
import { LOCALES, localePath, type Locale } from "@/config/locales";
import {
  ensureLanguageSchema,
  markLanguageChosen,
} from "@/lib/language-preference";
import { localizePath } from "@/lib/locale-path";
import { syncLanguageToAccount } from "@/lib/language-sync";
import { usePublicStatus } from "@/lib/public-status";

const LOCALE_OPTIONS = LOCALES.map((entry) => ({
  value: entry.code,
  label: entry.endonym,
  short: entry.shortLabel,
}));

/** Product links in the approved footer order (the demo's PRODUCT_ORDER). */
const PRODUCT_ORDER: readonly PublicNavKey[] = [
  "models",
  "pricing",
  "rankings",
  "about",
  "console",
];

const ENTITIES: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  "#39": "'",
  nbsp: " ",
};

/**
 * The site statement is operator HTML; the footer renders it as text, so tags
 * become spaces (not nothing) to keep two adjacent blocks readable.
 */
function plainText(html: string | undefined): string {
  return (html ?? "")
    .replace(/<(script|style)\b[^>]*>[\s\S]*?<\/\1>/gi, " ")
    .replace(/<[^>]*>/g, " ")
    .replace(/&(amp|lt|gt|quot|#39|nbsp);/gi, (match, name: string) => {
      return ENTITIES[name.toLowerCase()] ?? match;
    })
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * The copyable OpenAI-compatible base URL. Only a public absolute http(s)
 * address qualifies: loopback is never reachable by a visitor, so showing it
 * would be a lie dressed as data.
 */
function apiEndpoint(serverAddress: string): string {
  const value = serverAddress.trim().replace(/\/+$/, "");
  if (!/^https?:\/\//i.test(value)) return "";

  let host: string;
  try {
    host = new URL(value).hostname.toLowerCase();
  } catch {
    return "";
  }
  if (host === "localhost" || host === "127.0.0.1" || host === "::1" || host === "[::1]") {
    return "";
  }
  return `${value}/v1`;
}

/**
 * Marketing footer. Structure, styling and copy come from
 * `@newtg/public-shell`; this adapter only supplies the marketing link set —
 * the same shared labels the SPA uses, so both footers read identically.
 *
 * Every href is a real destination resolved from `GET /api/status`; the shell
 * owns the visibility rule (`isNavEntryVisible`), so a switch turned off in the
 * console disappears from both apps at once.
 */
export function SiteFooter({ copy, locale }: { copy: Copy; locale: Locale }) {
  const { status, nav, docsEnabled, docsUrl, statusPageUrl, serverAddress, siteTitle } =
    usePublicStatus();
  const shell = getShellCopy(locale);
  const homePath = localePath(locale);
  const siteName = siteTitle || copy.brand;

  // `/pricing` is the only localized sub-route the marketing site owns; the
  // footer reaches the same static page the header does.
  const pricingHref =
    homePath === "/" ? "/pricing/" : `${homePath}pricing/`;

  // The marketing site has no session, so every entry is resolved as a guest.
  const productLinks: ShellFooterLink[] = PRODUCT_ORDER.flatMap((key) => {
    if (!isNavEntryVisible(nav, key, false, docsUrl)) return [];
    return [
      {
        label: shell[NAV_COPY_KEY[key]],
        href: key === "pricing" ? pricingHref : PUBLIC_NAV_HREFS[key],
      },
    ];
  });

  const statusHref = statusPageUrl.trim();
  const resourceLinks: ShellFooterLink[] = [];
  if (docsEnabled && isSafePublicHref(docsUrl)) {
    resourceLinks.push({
      label: shell["nav.docs"],
      href: docsUrl,
      external: isExternalHref(docsUrl),
    });
  }
  if (isSafePublicHref(statusHref)) {
    resourceLinks.push({
      label: shell["footer.status"],
      href: statusHref,
      external: isExternalHref(statusHref),
    });
  }
  resourceLinks.push({ label: shell["footer.terms"], href: "/legal/terms" });
  resourceLinks.push({ label: shell["footer.privacy"], href: "/legal/privacy" });

  const columns: ShellFooterColumn[] = [];
  if (productLinks.length > 0) {
    columns.push({
      key: "product",
      title: shell["footer.product"],
      links: productLinks,
    });
  }
  columns.push({
    key: "resources",
    title: shell["footer.resources"],
    links: resourceLinks,
  });
  columns.push({
    key: "friends",
    title: shell["footer.friends"],
    links: FRIEND_LINKS,
  });

  const filingNo = (status?.site?.filing_no ?? "").trim();
  const statement = plainText(status?.site?.about_html) || shell["footer.tagline"];
  const endpoint = apiEndpoint(serverAddress);

  function changeLocale(next: string) {
    const value = next as Locale;
    ensureLanguageSchema();
    markLanguageChosen(value);
    void syncLanguageToAccount(value);
    window.location.assign(localizePath(window.location.pathname, value));
  }

  const year = new Date().getFullYear();
  const copyright = `${formatShellCopy(shell["footer.copyright"], { year, site: siteName })} ${shell["footer.all_rights"]}`;

  return (
    <ShellFooter
      copy={shell}
      brand={{ title: siteName, href: homePath }}
      statement={statement}
      endpoint={endpoint || undefined}
      columns={columns}
      locale={{
        value: locale,
        options: LOCALE_OPTIONS,
        onChange: changeLocale,
      }}
      copyright={copyright}
      filing={
        filingNo
          ? { label: filingNo, href: "https://beian.miit.gov.cn/" }
          : null
      }
    />
  );
}

export default SiteFooter;
