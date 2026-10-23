"use client";

import { useState } from "react";
import { copy } from "@/config/copy";
import { site } from "@/config/site";

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

export function SiteHeader() {
  const [open, setOpen] = useState(false);

  function closeMenu() {
    setOpen(false);
  }

  return (
    <header className="fixed inset-x-0 top-0 z-50 border-b border-line bg-[rgba(7,11,18,0.74)] backdrop-blur-[14px]">
      <div className="mx-auto flex h-16 w-full max-w-[1520px] items-center gap-9 px-4 min-[621px]:px-6 wide:px-10">
        <a
          href="#top"
          aria-label="NEWTG 首页"
          className="inline-flex flex-none items-center gap-[9px] text-ink"
        >
          <RoutingLogo className="text-cyan" />
          <span className="text-base font-bold tracking-[0.09em]">{copy.brand}</span>
        </a>

        <nav aria-label="主导航" className="mr-auto hidden items-center gap-7 nav:flex">
          {copy.nav.links.map((link) => (
            <a
              key={link.href}
              href={link.href}
              className="text-sm text-ink-2 transition-colors hover:text-ink"
            >
              {link.label}
            </a>
          ))}
        </nav>

        <div className="hidden flex-none items-center gap-3.5 nav:flex">
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

        <button
          type="button"
          aria-expanded={open}
          aria-controls="mobile-menu"
          aria-label={open ? copy.nav.closeMenu : copy.nav.openMenu}
          onClick={() => setOpen((value) => !value)}
          className="ml-auto inline-flex h-10 w-10 flex-none items-center justify-center rounded-md border border-line-2 bg-transparent text-ink nav:hidden"
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

      {open && (
        <nav
          id="mobile-menu"
          aria-label="主导航"
          className="border-t border-line bg-[rgba(7,11,18,0.97)] nav:hidden"
        >
          <div className="mx-auto w-full max-w-[1520px] px-4 pt-2.5 pb-[18px] min-[621px]:px-6 wide:px-10">
            {copy.nav.links.map((link) => (
              <a
                key={link.href}
                href={link.href}
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
