"use client";

import {
  ShellFooter,
  formatShellCopy,
  getShellCopy,
  type ShellFooterColumn,
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

/**
 * Marketing footer. Structure, styling and copy come from
 * `@newtg/public-shell`; this adapter only supplies the marketing link set —
 * the same shared labels the SPA uses, so both footers read identically.
 */
export function SiteFooter({ copy, locale }: { copy: Copy; locale: Locale }) {
  const { docsEnabled } = usePublicStatus();
  const shell = getShellCopy(locale);
  const homePath = localePath(locale);
  const hrefFor = (href: string): string =>
    href === "/pricing"
      ? homePath === "/"
        ? "/pricing/"
        : `${homePath}pricing/`
      : href;

  const statusLink = {
    label: shell["footer.status"],
    href: "#status",
    dot: true,
  };

  const columns: ShellFooterColumn[] = [
    {
      key: "product",
      title: shell["footer.product"],
      links: [
        { label: shell["nav.models"], href: hrefFor("/models") },
        { label: shell["nav.pricing"], href: hrefFor("/pricing") },
        { label: shell["nav.console"], href: hrefFor("/dashboard") },
        statusLink,
      ],
    },
    {
      key: "docs",
      title: shell["footer.docs_column"],
      links: [
        { label: shell["footer.quickstart"], href: "/docs" },
        { label: shell["footer.api_reference"], href: "/docs" },
        { label: shell["footer.sdks"], href: "/docs" },
        { label: shell["footer.changelog"], href: "/docs" },
      ],
    },
    {
      key: "community",
      title: shell["footer.community"],
      links: [
        { label: shell["footer.github"], href: "#community" },
        { label: shell["footer.discord"], href: "#community" },
        { label: shell["footer.x"], href: "#community" },
        { label: shell["footer.contact"], href: "#community" },
      ],
    },
    {
      key: "legal",
      title: shell["footer.legal"],
      links: [
        { label: shell["footer.terms"], href: "/legal/terms" },
        { label: shell["footer.privacy"], href: "/legal/privacy" },
      ],
    },
  ];

  // The docs column is entirely docs links; drop it whenever the deployment
  // has no live docs target.
  const visibleColumns = docsEnabled
    ? columns
    : columns.filter((column) => column.key !== "docs");

  function changeLocale(next: string) {
    const value = next as Locale;
    ensureLanguageSchema();
    markLanguageChosen(value);
    void syncLanguageToAccount(value);
    window.location.assign(localizePath(window.location.pathname, value));
  }

  const year = new Date().getFullYear();
  const copyright = `${formatShellCopy(shell["footer.copyright"], { year, site: copy.brand })} ${shell["footer.all_rights"]}`;

  return (
    <ShellFooter
      copy={shell}
      brand={{ title: copy.brand, href: "#top" }}
      tagline={shell["footer.tagline"]}
      columns={visibleColumns}
      locale={{
        value: locale,
        options: LOCALE_OPTIONS,
        onChange: changeLocale,
      }}
      copyright={copyright}
    />
  );
}

export default SiteFooter;
