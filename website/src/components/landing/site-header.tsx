"use client";

import { useEffect, useState } from "react";
import { usePathname } from "next/navigation";
import {
  ShellHeader,
  getShellCopy,
  type ShellAccountControl,
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
import { fetchHeaderSession, signOut, type HeaderSession } from "@/lib/session";
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

/**
 * Session state for the header. Mirrors the console: `/api/me` is the only
 * probe, a 401 is the guest answer, and any other failure also degrades to
 * guest — the marketing page never shows a session error in the chrome.
 */
function useHeaderSession(): HeaderSession {
  const [session, setSession] = useState<HeaderSession>({ signedIn: false });

  useEffect(() => {
    const controller = new AbortController();
    void fetchHeaderSession(controller.signal).then((next) => {
      if (!controller.signal.aborted) setSession(next);
    });
    return () => controller.abort();
  }, []);

  return session;
}

export function SiteHeader({ copy, locale }: { copy: Copy; locale: Locale }) {
  const { docsEnabled } = usePublicStatus();
  const pathname = usePathname() ?? "/";
  const theme = useThemeControl();
  const session = useHeaderSession();
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

  // Signed-in visitors see who they are instead of a sign-in link; the menu
  // hands them back to the console, which owns the account pages.
  const account: ShellAccountControl = session.signedIn
    ? {
        signedIn: true,
        signInHref: site.loginHref,
        name: session.name,
        menu: [
          { key: "console", label: shell["nav.console"], href: "/dashboard" },
          { key: "profile", label: shell["nav.profile"], href: "/profile" },
        ],
        onSignOut: () => void signOut(homePath),
      }
    : { signedIn: false, signInHref: site.loginHref };

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
      account={account}
    />
  );
}

export default SiteHeader;
