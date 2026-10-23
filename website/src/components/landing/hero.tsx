import { Fragment } from "react";
import { CodeCard } from "@/components/landing/code-card";
import { copy } from "@/config/copy";
import { site } from "@/config/site";
import { highlightCurl } from "@/lib/highlight";

export async function Hero() {
  const html = await highlightCurl(copy.hero.code);

  return (
    <section id="top" className="pt-28 pb-14 min-[1081px]:pt-[136px] min-[1081px]:pb-13">
      <div className="mx-auto grid w-full max-w-[1520px] items-center gap-14 px-4 min-[621px]:px-6 min-[1081px]:grid-cols-[1.02fr_0.98fr] wide:px-10">
        <div className="min-w-0">
          <h1 className="rise text-[clamp(2.5rem,4.6vw,4rem)] leading-[1.08] font-bold tracking-[-0.035em] text-balance">
            {copy.hero.titleLines.map((line, index) => (
              <Fragment key={line}>
                {index > 0 ? <br /> : null}
                {line}
              </Fragment>
            ))}
          </h1>

          <p className="rise rise-1 mt-[18px] max-w-[34em] text-base leading-[1.85] text-ink-2">
            {copy.hero.subtitle}
          </p>

          <div className="rise rise-2 mt-6 flex flex-wrap gap-3">
            <a className="btn btn-primary" href={site.startHref}>
              {copy.hero.primaryCta}
            </a>
            <a className="btn btn-ghost" href={site.docsHref}>
              {copy.hero.secondaryCta}
            </a>
          </div>

          <div className="rise rise-3 mt-5 flex flex-wrap items-center gap-x-3.5 gap-y-2 text-[12.5px] text-faint">
            <span>{copy.hero.compatLabel}</span>
            {copy.hero.compat.map((item, index) => (
              <Fragment key={item}>
                {index > 0 ? (
                  <span aria-hidden="true" className="text-line-2">
                    /
                  </span>
                ) : null}
                <code className="font-mono text-[11.5px] text-muted">{item}</code>
              </Fragment>
            ))}
          </div>
        </div>

        <figure className="rise rise-2 m-0 min-w-0">
          <CodeCard html={html} code={copy.hero.code} />
        </figure>
      </div>
    </section>
  );
}
