"use client";

import { useEffect, useState } from "react";
import { usePathname } from "next/navigation";
import {
  ShellHeader,
  getShellCopy,
  type ShellCopyKey,
  type ShellLocaleOption,
  type ShellNavItem,
} from "@newtg/public-shell";
import type { Copy } from "@/config/copy";
import { LOCALES, localePath, type Locale } from "@/config/locales";
import { site } from "@/config/site";
import {
  ensureLanguageSchema,
  markLanguageChosen,
} from "@/lib/language-preference";
import { localizePath } from "@/lib/locale-path";
import { syncLanguageToAccount } from "@/lib/language-sync";
import { usePublicStatus } from "@/lib/public-status";
import { applyTheme, readStoredTheme, storeTheme } from "@/lib/theme";

/**
 * Same six entries, same order and same labels as the SPA header (both read
 * the shared shell dictionary). The static site cannot see the operator's
 * `nav.*` switches, so only the docs target is filtered — it is the one entry
 * that depends on a deployed target.
 */
const NAV_ITEMS: readonly { key: string; labelKey: ShellCopyKey; href: string }[] = [
  { key: "console", labelKey: "nav.console", href: "/dashboard" },
  { key: "models", labelKey: "nav.models", href: "/models" },
  { key: "pricing", labelKey: "nav.pricing", href: "/pricing" },
  { key: "rankings", labelKey: "nav.rankings", href: "/rankings" },
  { key: "about", labelKey: "nav.about", href: "/about" },
  { key: "docs", labelKey: "nav.docs", href: "/docs" },
];

const LOCALE_OPTIONS: ShellLocaleOption[] = LOCALES.map((entry) => ({
  value: entry.code,
  label: entry.endonym,
  short: entry.shortLabel,
}));

/**
 * Theme state for the shell. The pre-paint script has already set
 * `data-theme`; this hook mirrors it into React and keeps following the OS
 * while the visitor has made no explicit choice (the same contract the
 * marketing toggle always had).
 */
function useThemeControl(): { value: "light" | "dark"; onToggle: () => void } {
  const [value, setValue] = useState<"light" | "dark">("light");

  useEffect(() => {
    const read = (): "light" | "dark" =>
      document.documentElement.dataset.theme === "dark" ? "dark" : "light";
    setValue(read());

    const media = window.matchMedia?.("(prefers-color-scheme: dark)");
    if (!media) return;
    const onChange = (event: MediaQueryListEvent) => {
      // An explicit choice wins; only follow the OS when nothing is stored.
      if (readStoredTheme() !== null) return;
      const next = event.matches ? "dark" : "light";
      applyTheme(next);
      setValue(next);
    };
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, []);

  function onToggle() {
    const next = value === "dark" ? "light" : "dark";
    applyTheme(next);
    storeTheme(next);
    setValue(next);
  }

  return { value, onToggle };
}

export function SiteHeader({ copy, locale }: { copy: Copy; locale: Locale }) {
  const { docsEnabled } = usePublicStatus();
  const pathname = usePathname() ?? "/";
  const theme = useThemeControl();
  const shell = getShellCopy(locale);

  const homePath = localePath(locale);
  const normalized = pathname.replace(/\/$/, "") || "/";
  const onHome = normalized === (homePath.replace(/\/$/, "") || "/");

  function hrefFor(href: string): string {
    // '/pricing' is the only localized sub-route the marketing site owns.
    return href === "/pricing"
      ? homePath === "/"
        ? "/pricing/"
        : `${homePath}pricing/`
      : href;
  }

  const items: ShellNavItem[] = NAV_ITEMS.filter(
    (item) => item.href !== "/docs" || docsEnabled,
  ).map((item) => ({
    key: item.key,
    label: shell[item.labelKey],
    href: hrefFor(item.href),
    current: normalized === hrefFor(item.href).replace(/\/$/, ""),
  }));

  function changeLocale(next: string) {
    const value = next as Locale;
    ensureLanguageSchema();
    markLanguageChosen(value);
    // Background write through keepalive so the pick survives the navigation.
    void syncLanguageToAccount(value);
    // Keep the current page, swapping only the locale prefix ('/' is zh-CN).
    window.location.assign(localizePath(window.location.pathname, value));
  }

  return (
    <ShellHeader
      copy={shell}
      brand={{
        title: copy.brand,
        // On the home page the wordmark returns to the top, as it always has.
        href: onHome ? "#top" : homePath,
      }}
      items={items}
      locale={{ value: locale, options: LOCALE_OPTIONS, onChange: changeLocale }}
      theme={theme}
      account={{ signedIn: false, signInHref: site.loginHref }}
    />
  );
}

export default SiteHeader;
