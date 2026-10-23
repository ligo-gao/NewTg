import { copy } from "@/config/copy";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

function cellClasses(index: number): string {
  if (index === 0) {
    return "py-5 pr-6 pl-0";
  }
  if (index === 1) {
    return "border-l border-line px-6 py-5";
  }
  if (index === 2) {
    // Second row below nav: add a rule between rows; at 4 columns it becomes a left rule instead.
    return "border-t border-line py-5 pr-6 pl-0 nav:border-t-0 nav:border-l nav:border-line nav:pl-6";
  }
  return "border-t border-l border-line px-6 py-5 nav:border-t-0";
}

export function TrustBand() {
  return (
    <section aria-label={copy.trust.ariaLabel} className="border-y border-line">
      <div className={container}>
        <dl className="grid grid-cols-2 nav:grid-cols-4">
          {copy.trust.items.map((item, index) => (
            <div key={item.value} className={cellClasses(index)}>
              <dt className="font-mono text-[clamp(1.5rem,2.4vw,2rem)] leading-[1.1] font-medium tracking-[-0.02em] text-ink tabular-nums">
                {item.value}
              </dt>
              <dd className="mt-1.5 text-[12.5px] text-muted">{item.label}</dd>
            </div>
          ))}
        </dl>
        <p className="pb-2.5 text-right text-[11px] text-faint">{copy.trust.note}</p>
      </div>
    </section>
  );
}
