"use client";

import { useHomeData } from "@/components/landing/home-data";
import type { Copy } from "@/config/copy";
import { formatTTFT, formatTPS } from "@/lib/pricing-format";

/** 只认最近一次实测；超过 36 小时视为无数据（容许漏掉一个时段再加余量）。 */
const PERF_FRESH_MS = 36 * 60 * 60 * 1000;

function SkeletonValue() {
  return <span className="inline-block h-3 w-16 animate-pulse rounded bg-line-2/50" />;
}

export function PerformanceSection({ copy }: { copy: Copy }) {
  const { catalogStatus, metricsStatus, perfFeatured, metrics, retry } = useHomeData();
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
              className="text-[clamp(1.75rem,3vw,2.4rem)] font-semibold leading-tight tracking-[-0.03em]"
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
        ) : !loadingCatalog && perfFeatured.length === 0 ? (
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
                    : perfFeatured.map((model) => {
                        const entry = metrics[model.model_id];
                        const latest = entry?.latest_test ?? null;
                        // 只认最近一次实测：超过 36h 视为无数据（容许漏掉一个时段再加余量），
                        // 未来时间戳（时钟偏移）仍按新鲜处理。
                        const testedAt = latest?.tested_at
                          ? Date.parse(latest.tested_at)
                          : Number.NaN;
                        const fresh =
                          Number.isFinite(testedAt) && Date.now() - testedAt <= PERF_FRESH_MS;
                        const ttft = fresh ? (latest?.ttft_ms_p50 ?? null) : null;
                        const tps = fresh ? (latest?.tps ?? null) : null;
                        return (
                          <tr
                            key={model.model_id}
                            className="transition-colors hover:bg-[var(--hover-tint)]"
                          >
                            <td className="border-b border-line px-5 py-3.5">
                              <span className="font-mono text-[12.5px] text-ink">
                                {model.display_name || model.model_id}
                              </span>
                            </td>
                            <td className="border-b border-line px-5 py-3.5 text-muted">
                              {model.vendor}
                            </td>
                            <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] tabular-nums text-ink-2">
                              {loadingMetrics && !entry ? (
                                <SkeletonValue />
                              ) : (
                                formatTTFT(ttft)
                              )}
                            </td>
                            <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] tabular-nums text-ink-2">
                              {loadingMetrics && !entry ? (
                                <SkeletonValue />
                              ) : (
                                formatTPS(tps)
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
