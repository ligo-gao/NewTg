"use client";

import { useHomeData } from "@/components/landing/home-data";
import type { Copy } from "@/config/copy";
import { formatTTFT, formatTPS, isLowSample } from "@/lib/pricing-format";

function SkeletonValue() {
  return <span className="inline-block h-3 w-16 animate-pulse rounded bg-line-2/50" />;
}

export function PerformanceSection({ copy }: { copy: Copy }) {
  const { catalogStatus, metricsStatus, featured, metrics, retry } = useHomeData();
  const loadingCatalog = catalogStatus === "loading";
  const loadingMetrics = metricsStatus === "loading";

  return (
    <section
      id="performance"
      className="scroll-mt-20 border-t border-line py-16"
      aria-labelledby="performance-title"
    >
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className="mb-7 flex flex-wrap items-end justify-between gap-x-8 gap-y-4">
          <div>
            <h2
              id="performance-title"
              className="text-[clamp(1.7rem,3vw,2.4rem)] font-semibold leading-tight tracking-[-0.03em]"
            >
              {copy.performance.title}
            </h2>
            <p className="mt-2.5 max-w-[38em] text-[14.5px] text-muted">
              {copy.performance.subtitle}
            </p>
          </div>
          <span className="rounded-full border border-line px-[13px] py-1 font-mono text-[11.5px] text-muted">
            {copy.performance.windowLabel}
          </span>
        </div>

        {catalogStatus === "error" ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted">
            <p>{copy.models.loadError}</p>
            <button
              type="button"
              onClick={retry}
              className="mt-3 text-[13px] text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
            >
              {copy.performance.retry}
            </button>
          </div>
        ) : !loadingCatalog && featured.length === 0 ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted">
            {copy.performance.empty}
          </div>
        ) : (
          <>
            {metricsStatus === "error" ? (
              <div className="mb-3 flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-line bg-bg-2 px-4 py-2.5 text-[12.5px] text-muted">
                <span>{copy.performance.unavailable}</span>
                <button
                  type="button"
                  onClick={retry}
                  className="text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
                >
                  {copy.performance.retry}
                </button>
              </div>
            ) : null}

            <div className="overflow-x-auto rounded-[10px] border border-line bg-bg-2">
              <table className="w-full min-w-[640px] border-collapse text-[13.5px]">
                <caption className="sr-only">{copy.performance.subtitle}</caption>
                <thead>
                  <tr>
                    <th
                      scope="col"
                      className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                    >
                      {copy.performance.columns.model}
                    </th>
                    <th
                      scope="col"
                      className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                    >
                      {copy.performance.columns.vendor}
                    </th>
                    <th
                      scope="col"
                      className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                    >
                      {copy.performance.columns.ttft}
                    </th>
                    <th
                      scope="col"
                      className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                    >
                      {copy.performance.columns.tps}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {loadingCatalog
                    ? Array.from({ length: 6 }).map((_, index) => (
                        <tr key={index}>
                          <td className="border-b border-line px-5 py-3.5">
                            <SkeletonValue />
                          </td>
                          <td className="border-b border-line px-5 py-3.5">
                            <SkeletonValue />
                          </td>
                          <td className="border-b border-line px-5 py-3.5">
                            <SkeletonValue />
                          </td>
                          <td className="border-b border-line px-5 py-3.5">
                            <SkeletonValue />
                          </td>
                        </tr>
                      ))
                    : featured.map((model) => {
                        const summary = metrics[model.model_id]?.summary;
                        const sampleSize = summary?.sample_size ?? 0;
                        return (
                          <tr
                            key={model.model_id}
                            className="transition-colors hover:bg-[var(--hover-tint)]"
                          >
                            <td className="border-b border-line px-5 py-3.5">
                              <div className="flex flex-wrap items-center gap-2">
                                <span className="font-mono text-[12.5px] text-ink">
                                  {model.display_name || model.model_id}
                                </span>
                                {isLowSample(sampleSize) ? (
                                  <span className="rounded-[4px] border border-amber/35 px-1.5 py-px font-mono text-[10px] text-amber">
                                    {copy.performance.lowSample}
                                  </span>
                                ) : null}
                              </div>
                            </td>
                            <td className="border-b border-line px-5 py-3.5 text-muted">
                              {model.vendor}
                            </td>
                            <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] tabular-nums text-ink-2">
                              {loadingMetrics && !summary ? (
                                <SkeletonValue />
                              ) : (
                                formatTTFT(summary?.avg_ttft_ms ?? null)
                              )}
                            </td>
                            <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] tabular-nums text-ink-2">
                              {loadingMetrics && !summary ? (
                                <SkeletonValue />
                              ) : (
                                formatTPS(summary?.tps ?? null)
                              )}
                            </td>
                          </tr>
                        );
                      })}
                </tbody>
              </table>
            </div>
          </>
        )}

        <p className="mt-3.5 text-[11.5px] text-faint">{copy.performance.footnote}</p>
      </div>
    </section>
  );
}
