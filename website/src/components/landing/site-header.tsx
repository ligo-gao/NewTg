"use client";

import { useState } from "react";
import { usePathname } from "next/navigation";
import { LanguageSwitcher } from "@/components/language-switcher";
import { ThemeToggle } from "@/components/theme-toggle";
import type { Copy } from "@/config/copy";
import { localePath, type Locale } from "@/config/locales";
import { site } from "@/config/site";
import { usePublicStatus } from "@/lib/public-status";

const DOCS_HREF = "/docs";

function RoutingLogo({ className }: { className?: string }) {
  return (
    <svg
      width="22"
      height="22"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      aria-hidden="true"
      className={className}
    >
      <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
      <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
    </svg>
  );
}

export function SiteHeader({ copy, locale }: { copy: Copy; locale: Locale }) {
  const [open, setOpen] = useState(false);
  const { docsEnabled } = usePublicStatus();
  const pathname = usePathname() ?? "/";
  const homePath = locale === "zh-CN" ? "/" : localePath(locale);
  const pricingPath = homePath === "/" ? "/pricing/" : `${homePath}pricing/`;
  const normalized = pathname.replace(/\/$/, "") || "/";
  const homeNormalized = homePath.replace(/\/$/, "") || "/";
  const onHome = normalized === homeNormalized;
  const onPricing = normalized === pricingPath.replace(/\/$/, "");

  function hrefFor(href: string): string {
    if (href === "/pricing") return pricingPath;
    return href;
  }

  function closeMenu() {
    setOpen(false);
  }

  // One filtered list feeds both menus so the docs entry stays consistent.
  const navLinks = docsEnabled
    ? copy.nav.links
    : copy.nav.links.filter((link) => link.href !== DOCS_HREF);

  return (
    <header className="fixed inset-x-0 top-0 z-50 border-b border-line nav-surface backdrop-blur-[14px]">
      <div className="mx-auto flex h-16 w-full max-w-[1520px] items-center gap-9 px-4 min-[621px]:px-6 wide:px-10">
        <a
          href={onHome ? "#top" : homePath}
          aria-label={copy.brand}
          className="inline-flex flex-none items-center gap-[9px] text-ink"
        >
          <RoutingLogo className="text-cyan" />
          <span className="text-base font-bold tracking-[0.09em]">{copy.brand}</span>
        </a>

        <nav aria-label="主导航" className="mr-auto hidden items-center gap-7 nav:flex">
          {navLinks.map((link) => (
            <a
              key={link.href}
              href={hrefFor(link.href)}
              aria-current={onPricing && link.href === "/pricing" ? "page" : undefined}
              className="text-sm text-ink-2 transition-colors hover:text-ink aria-[current=page]:text-ink"
            >
              {link.label}
            </a>
          ))}
        </nav>

        <div className="hidden flex-none items-center gap-3.5 nav:flex">
          <LanguageSwitcher locale={locale} label={copy.language.label} />
          <ThemeToggle copy={copy} />
          <a
            href={site.loginHref}
            className="text-sm text-ink-2 transition-colors hover:text-ink"
          >
            {copy.nav.login}
          </a>
          <a href={site.startHref} className="btn btn-primary btn-sm">
            {copy.nav.cta}
          </a>
        </div>

        <div className="ml-auto flex items-center gap-2 nav:hidden">
          <LanguageSwitcher locale={locale} label={copy.language.label} compact />
          <ThemeToggle copy={copy} />
          <button
            type="button"
            aria-expanded={open}
            aria-controls="mobile-menu"
            aria-label={open ? copy.nav.closeMenu : copy.nav.openMenu}
            onClick={() => setOpen((value) => !value)}
            className="inline-flex h-10 w-10 flex-none items-center justify-center rounded-md border border-line-2 bg-transparent text-ink"
          >
          <svg
            width="16"
            height="16"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            aria-hidden="true"
          >
            {open ? (
              <path d="M3 3l10 10M13 3L3 13" />
            ) : (
              <path d="M2 4h12M2 8h12M2 12h12" />
            )}
          </svg>
          </button>
        </div>
      </div>

      {open && (
        <nav
          id="mobile-menu"
          aria-label="主导航"
          className="border-t border-line menu-surface nav:hidden"
        >
          <div className="mx-auto w-full max-w-[1520px] px-4 pt-2.5 pb-[18px] min-[621px]:px-6 wide:px-10">
            {navLinks.map((link) => (
              <a
                key={link.href}
                href={hrefFor(link.href)}
                aria-current={onPricing && link.href === "/pricing" ? "page" : undefined}
                onClick={closeMenu}
                className="block border-b border-line py-3 text-[15px] text-ink-2"
              >
                {link.label}
              </a>
            ))}
            <a
              href={site.loginHref}
              onClick={closeMenu}
              className="block border-b border-line py-3 text-[15px] text-ink-2"
            >
              {copy.nav.login}
            </a>
            <div className="flex items-center justify-between border-b border-line py-3">
              <span className="text-[15px] text-ink-2">{copy.language.label}</span>
              <LanguageSwitcher locale={locale} label={copy.language.label} />
            </div>
            <a
              href={site.startHref}
              onClick={closeMenu}
              className="btn btn-primary mt-3 w-full"
            >
              {copy.nav.cta}
            </a>
          </div>
        </nav>
      )}
    </header>
  );
}

export default SiteHeader;
