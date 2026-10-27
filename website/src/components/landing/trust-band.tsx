import type { Copy } from "@/config/copy";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

function cellClasses(index: number): string {
  if (index === 0) {
    return "min-w-0 py-3.5 pr-6 pl-0";
  }
  if (index === 1) {
    return "min-w-0 border-l border-line px-6 py-3.5";
  }
  if (index === 2) {
    // Second row below nav: add a rule between rows; at 4 columns it becomes a left rule instead.
    return "min-w-0 border-t border-line py-3.5 pr-6 pl-0 nav:border-t-0 nav:border-l nav:border-line nav:pl-6";
  }
  return "min-w-0 border-t border-l border-line px-6 py-3.5 nav:border-t-0";
}

export function TrustBand({ copy }: { copy: Copy }) {
  return (
    <section aria-label={copy.trust.ariaLabel} className="border-y border-line">
      <div className={container}>
        <dl className="grid grid-cols-2 nav:grid-cols-4">
          {copy.trust.items.map((item, index) => (
            <div key={item.value} className={cellClasses(index)}>
              {/*
                360–390px: the 2-column track gives each cell ~164px, and long
                unbreakable values (Маршрутизация, Facturation, ルーティング)
                used to widen the track and scroll the page. `min-w-0` on the
                cell lets the value shrink; `overflow-wrap: anywhere` lets it
                break inside a word (CJK breaks naturally). Desktop scale is
                unchanged.
              */}
              <dt className="font-mono text-[clamp(1.5rem,2.4vw,2rem)] leading-[1.1] font-medium tracking-[-0.02em] text-ink tabular-nums [overflow-wrap:anywhere] hyphens-auto">
                {item.value}
              </dt>
              <dd className="mt-1.5 text-[12.5px] text-muted">{item.label}</dd>
            </div>
          ))}
        </dl>
      </div>
    </section>
  );
}
