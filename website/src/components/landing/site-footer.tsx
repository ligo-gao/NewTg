import type { Copy } from "@/config/copy";

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

export function SiteFooter({ copy }: { copy: Copy }) {
  return (
    <footer className="border-t border-line pt-11 pb-8 text-[13px]">
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className="grid grid-cols-2 gap-8 nav:grid-cols-[1.6fr_repeat(4,1fr)] nav:gap-10">
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

          {copy.footer.columns.map((column, index) => (
            <div key={column.title} className="min-w-0">
              <h3 className="mb-3.5 text-xs font-medium text-faint">{column.title}</h3>
              <ul>
                {column.links.map((link) => (
                  <li key={`${column.title}-${link.label}`}>
                    <a
                      href={link.href}
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
