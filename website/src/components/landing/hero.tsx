import { Fragment } from "react";
import { CodeCard, type CodeCardSample } from "@/components/landing/code-card";
import { HeroDocsCta } from "@/components/landing/hero-docs-cta";
import { codeSamples } from "@/config/code-samples";
import type { Copy } from "@/config/copy";
import { site } from "@/config/site";
import { highlightCode } from "@/lib/highlight";

export async function Hero({ copy }: { copy: Copy }) {
  const samples: CodeCardSample[] = await Promise.all(
    codeSamples(copy.hero.prompt).map(async (sample) => ({
      ...sample,
      html: await highlightCode(sample.code, sample.lang),
    })),
  );

  return (
    <section
      id="top"
      className="pt-[104px] pb-7 min-[621px]:pt-28 min-[621px]:pb-8 min-[1081px]:pt-[120px]"
    >
      <div className="mx-auto grid w-full max-w-[1520px] items-center gap-14 px-4 min-[621px]:px-6 min-[1081px]:grid-cols-[1.02fr_0.98fr] wide:px-10">
        <div className="min-w-0">
          <h1 className="rise text-public-display tracking-[-0.03em] text-balance">
            {copy.hero.titleLines.map((line, index) => (
              <Fragment key={line}>
                {index > 0 ? <br /> : null}
                {line}
              </Fragment>
            ))}
          </h1>

          <p className="rise rise-1 mt-[18px] max-w-[34em] text-public-body text-ink-2">
            {copy.hero.subtitle}
          </p>

          <div className="rise rise-2 mt-6 flex flex-wrap gap-3">
            <a className="btn btn-primary" href={site.startHref}>
              {copy.hero.primaryCta}
            </a>
            <HeroDocsCta copy={copy} />
          </div>

          <div className="rise rise-3 mt-5 flex flex-wrap items-center gap-x-3.5 gap-y-2 text-public-caption text-faint">
            <span>{copy.hero.compatLabel}</span>
            {copy.hero.compat.map((item, index) => (
              <Fragment key={item}>
                {index > 0 ? (
                  <span aria-hidden="true" className="text-line-2">
                    /
                  </span>
                ) : null}
                <code className="font-mono text-public-caption text-muted">{item}</code>
              </Fragment>
            ))}
          </div>
        </div>

        <figure className="rise rise-2 m-0 min-w-0">
          <CodeCard copy={copy} samples={samples} />
        </figure>
      </div>
    </section>
  );
}
