import type { Copy } from "@/config/copy";
import { site } from "@/config/site";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

export function ClosingCta({ copy }: { copy: Copy }) {
  return (
    <section
      id="start"
      className="scroll-mt-20 border-t border-line pt-10 pb-11 min-[621px]:pt-11 min-[621px]:pb-12"
    >
      <div className={container}>
        <div className="max-w-[780px]">
          <h2 className="text-public-section tracking-[-0.01em] text-balance">
            {copy.closing.title}
          </h2>
          <a href={site.startHref} className="btn btn-primary mt-6 h-12 px-[26px] text-public-body">
            {copy.closing.cta}
          </a>
          <p className="mt-3.5 text-public-caption text-faint">{copy.closing.note}</p>
        </div>
      </div>
    </section>
  );
}
