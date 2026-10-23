import type { ReactNode } from "react";
import type { Copy, FeatureIconKey } from "@/config/copy";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

const icons: Record<FeatureIconKey, ReactNode> = {
  router: (
    <svg
      viewBox="0 0 40 40"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      className="h-[34px] w-[34px]"
    >
      <path d="M4 20h10" />
      <circle cx="14" cy="20" r="2" />
      <path d="M16 20h6c2 0 3-2 4-5l1-2c1-3 2-5 4-5h4" />
      <path d="M16 20h6c2 0 3 2 4 5l1 2c1 3 2 5 4 5h4" />
      <circle cx="35" cy="8" r="1.8" />
      <circle cx="35" cy="32" r="1.8" />
      <circle cx="35" cy="20" r="1.8" />
    </svg>
  ),
  failover: (
    <svg
      viewBox="0 0 40 40"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      className="h-[34px] w-[34px]"
    >
      <rect x="4" y="10" width="12" height="20" rx="2" />
      <path d="m7 13 6 14" />
      <path d="M18 20h7" />
      <path d="m22 16 4 4-4 4" />
      <rect x="29" y="10" width="8" height="20" rx="2" />
      <path d="M33 16v8" />
    </svg>
  ),
  plug: (
    <svg
      viewBox="0 0 40 40"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      className="h-[34px] w-[34px]"
    >
      <path d="M14 8H8v24h6" />
      <path d="M26 8h6v24h-6" />
      <path d="M17 20h6" />
      <circle cx="20" cy="20" r="2" />
    </svg>
  ),
  gauge: (
    <svg
      viewBox="0 0 40 40"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      className="h-[34px] w-[34px]"
    >
      <path d="M6 30a14 14 0 0 1 28 0" />
      <path d="M20 30 27 19" />
      <circle cx="20" cy="30" r="2" />
      <path d="M6 30h4M30 30h4" />
    </svg>
  ),
};

export function FeatureGrid({ copy }: { copy: Copy }) {
  return (
    <section id="features" className="scroll-mt-20 border-t border-line py-16">
      <div className={container}>
        <div className="mb-7">
          <h2 className="text-[clamp(1.7rem,3vw,2.4rem)] leading-[1.25] font-semibold tracking-[-0.03em]">
            {copy.features.title}
          </h2>
          <p className="mt-2.5 max-w-[34em] text-[14.5px] text-muted">
            {copy.features.subtitle}
          </p>
        </div>

        <div className="grid grid-cols-1 gap-px overflow-hidden rounded-lg border border-line bg-line nav:grid-cols-2 wide:grid-cols-4">
          {copy.features.items.map((item) => (
            <article
              key={item.title}
              className="grid grid-cols-[40px_minmax(0,1fr)] gap-4 bg-bg p-7 transition-colors hover:bg-bg-2 wide:px-6 wide:py-[26px]"
            >
              <div className="pt-0.5 text-cyan" aria-hidden="true">
                {icons[item.icon]}
              </div>
              <div>
                <h3 className="text-base font-semibold tracking-[-0.01em]">
                  {item.title}
                </h3>
                <p className="mt-2 text-[13.5px] leading-[1.75] text-muted text-balance">
                  {item.description}
                </p>
              </div>
            </article>
          ))}
        </div>
      </div>
    </section>
  );
}
