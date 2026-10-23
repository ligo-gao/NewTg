"use client";

import type { Copy } from "@/config/copy";
import { localePath, type Locale } from "@/config/locales";
import { usePublicStatus } from "@/lib/public-status";

const DOCS_HREF = "/docs";

function RoutingLogo() {
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
    >
      <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
      <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
    </svg>
  );
}

export function SiteFooter({ copy, locale }: { copy: Copy; locale: Locale }) {
  const { docsEnabled } = usePublicStatus();
  const homePath = locale === "zh-CN" ? "/" : localePath(locale);
  const pricingPath = homePath === "/" ? "/pricing/" : `${homePath}pricing/`;
  const hrefFor = (href: string): string => {
    if (href === "#models") return `${homePath}#models`;
    if (href === "/pricing") return pricingPath;
    return href;
  };
  // The docs column is entirely docs links; drop them (and an emptied column)
  // whenever the deployment has no live docs target.
  const columns = docsEnabled
    ? copy.footer.columns
    : copy.footer.columns
        .map((column) => ({
          ...column,
          links: column.links.filter((link) => link.href !== DOCS_HREF),
        }))
        .filter((column) => column.links.length > 0);
  // Keeps the link columns balanced once the docs column is gone.
  const gridClass =
    columns.length >= 4
      ? "nav:grid-cols-[1.6fr_repeat(4,1fr)]"
      : columns.length === 3
        ? "nav:grid-cols-[1.6fr_repeat(3,1fr)]"
        : "nav:grid-cols-[1.6fr_repeat(2,1fr)]";
  return (
    <footer className="border-t border-line pt-11 pb-8 text-[13px]">
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className={`grid grid-cols-2 gap-8 nav:gap-10 ${gridClass}`}>
          <div className="col-span-2 nav:col-span-1">
            <a
              href="#top"
              aria-label="NEWTG 首页"
              className="inline-flex items-center gap-[9px] text-ink"
            >
              <RoutingLogo />
              <span className="text-base font-bold tracking-[0.09em]">{copy.brand}</span>
            </a>
            <p className="mt-3 max-w-[26em] text-[13px] text-muted">{copy.footer.tagline}</p>
          </div>

          {columns.map((column, index) => (
            <div key={column.title} className="min-w-0">
              <h3 className="mb-3.5 text-xs font-medium text-faint">{column.title}</h3>
              <ul>
                {column.links.map((link) => (
                  <li key={`${column.title}-${link.label}`}>
                    <a
                      href={hrefFor(link.href)}
                      className="block py-[5px] text-[13px] text-ink-2 transition-colors hover:text-ink"
                    >
                      {link.label}
                    </a>
                  </li>
                ))}
                {index === 0 && (
                  <li>
                    <a
                      href={copy.footer.status.href}
                      className="inline-flex items-center gap-[7px] py-[5px] text-[13px] text-ink-2 transition-colors hover:text-ink"
                    >
                      <span
                        aria-hidden="true"
                        className="h-1.5 w-1.5 flex-none rounded-full bg-green"
                      />
                      {copy.footer.status.label}
                    </a>
                  </li>
                )}
              </ul>
            </div>
          ))}
        </div>

        <div className="mt-8 border-t border-line pt-5 text-xs text-faint">
          <span>{copy.footer.copyright}</span>
        </div>
      </div>
    </footer>
  );
}

export default SiteFooter;
