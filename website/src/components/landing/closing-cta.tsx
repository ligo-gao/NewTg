import type { Copy } from "@/config/copy";
import { site } from "@/config/site";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

export function ClosingCta({ copy }: { copy: Copy }) {
  return (
    <section id="start" className="scroll-mt-20 border-t border-line py-20 sm:py-22">
      <div className={container}>
        <div className="max-w-[780px]">
          <h2 className="text-[clamp(2rem,4.2vw,3.3rem)] leading-[1.18] font-bold tracking-[-0.035em] text-balance">
            {copy.closing.title}
          </h2>
          <a href={site.startHref} className="btn btn-primary mt-6 h-12 px-[26px] text-[15px]">
            {copy.closing.cta}
          </a>
          <p className="mt-3 text-[12.5px] text-muted">{copy.closing.note}</p>
        </div>
      </div>
    </section>
  );
}
