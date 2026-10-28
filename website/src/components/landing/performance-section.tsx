"use client";

import { useHomeData } from "@/components/landing/home-data";
import type { Copy } from "@/config/copy";
import type { Locale } from "@/config/locales";
import {
  formatLatencyMs,
  formatSampleSize,
  formatSuccessRate,
  formatTestedAt,
  formatTps,
  successTone,
  type SuccessTone,
} from "@/lib/perf-format";

/** 只认最近一次实测；超过 36 小时视为无数据（容许漏掉一个时段再加余量）。 */
const PERF_FRESH_MS = 36 * 60 * 60 * 1000;

const PLACEHOLDER = "—";

const headCell =
  "border-b border-line px-4 py-3 text-public-caption font-medium whitespace-nowrap text-faint";
const groupCell =
  "border-l border-line px-4 pt-3 pb-1 text-center text-public-caption font-medium whitespace-nowrap text-faint";
const subCell =
  "border-b border-line px-4 pt-0 pb-2 text-right text-public-caption font-medium whitespace-nowrap text-faint";
const bodyCell = "border-b border-line px-4 py-3.5";
const numCell = `${bodyCell} text-right font-mono text-public-caption tabular-nums text-ink-2 whitespace-nowrap`;

const successToneClass: Record<SuccessTone, string> = {
  good: "text-green",
  plain: "text-ink-2",
  warn: "text-amber",
  missing: "text-ink-2",
};

function SkeletonValue() {
  return <span className="inline-block h-3 w-14 animate-pulse rounded bg-line-2/50" />;
}

export function PerformanceSection({
  copy,
  locale = "zh-CN",
}: {
  copy: Copy;
  locale?: Locale;
}) {
  const { catalogStatus, metricsStatus, perfFeatured, metrics, retry } = useHomeData();
  const loadingCatalog = catalogStatus === "loading";
  const loadingMetrics = metricsStatus === "loading";
  const columns = copy.performance.columns;
  // 后台没填 vendor 时整列都会是空白，留一个空表头比直接省掉这列更难看。
  const showVendor = perfFeatured.some((model) => model.vendor.trim() !== "");
  const columnCount = showVendor ? 10 : 9;

  return (
    <section
      id="performance"
      className="scroll-mt-20 border-t border-line py-8 min-[621px]:py-10"
      aria-labelledby="performance-title"
    >
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className="mb-[18px] flex flex-wrap items-end justify-between gap-x-8 gap-y-4">
          <h2
            id="performance-title"
            className="text-public-section tracking-[-0.01em]"
          >
            {copy.performance.title}
          </h2>
          <span className="rounded-full border border-line px-[13px] py-1 font-mono text-public-caption text-muted">
            {copy.performance.windowLabel}
          </span>
        </div>

        {catalogStatus === "error" ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-public-body text-muted">
            <p>{copy.models.loadError}</p>
            <button
              type="button"
              onClick={retry}
              className="mt-3 text-public-caption text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
            >
              {copy.performance.retry}
            </button>
          </div>
        ) : !loadingCatalog && perfFeatured.length === 0 ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-public-body text-muted">
            {copy.performance.empty}
          </div>
        ) : (
          <>
            {metricsStatus === "error" ? (
              <div className="mb-3 flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-line bg-bg-2 px-4 py-2.5 text-public-caption text-muted">
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
              <table className="w-full min-w-[1080px] border-collapse text-public-caption">
                <caption className="sr-only">{copy.performance.title}</caption>
                <thead>
                  <tr>
                    <th scope="col" rowSpan={2} className={`${headCell} text-left`}>
                      {columns.model}
                    </th>
                    {showVendor ? (
                      <th scope="col" rowSpan={2} className={`${headCell} text-left`}>
                        {columns.vendor}
                      </th>
                    ) : null}
                    <th scope="colgroup" colSpan={2} className={groupCell}>
                      {columns.ttftGroup}
                    </th>
                    <th scope="colgroup" colSpan={2} className={groupCell}>
                      {columns.latencyGroup}
                    </th>
                    <th scope="col" rowSpan={2} className={`${headCell} text-right`}>
                      {columns.tps}
                    </th>
                    <th scope="col" rowSpan={2} className={`${headCell} text-right`}>
                      {columns.successRate}
                    </th>
                    <th scope="col" rowSpan={2} className={`${headCell} text-right`}>
                      {columns.sampleSize}
                    </th>
                    <th scope="col" rowSpan={2} className={`${headCell} text-right`}>
                      {columns.testedAt}
                    </th>
                  </tr>
                  <tr>
                    <th scope="col" className={subCell}>
                      P50
                    </th>
                    <th scope="col" className={subCell}>
                      P95
                    </th>
                    <th scope="col" className={subCell}>
                      P50
                    </th>
                    <th scope="col" className={subCell}>
                      P95
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {loadingCatalog
                    ? Array.from({ length: 8 }).map((_, index) => (
                        <tr key={index}>
                          {Array.from({ length: columnCount }).map((__, cellIndex) => (
                            <td key={cellIndex} className={bodyCell}>
                              {cellIndex > (showVendor ? 1 : 0) ? <SkeletonValue /> : null}
                            </td>
                          ))}
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
                        const rate = fresh ? (latest?.success_rate ?? null) : null;
                        const rateTone = successToneClass[successTone(rate)];
                        const pending = loadingMetrics && !entry;

                        return (
                          <tr
                            key={model.model_id}
                            className="transition-colors hover:bg-[var(--hover-tint)]"
                          >
                            <td className={bodyCell}>
                              <span className="font-mono text-public-caption text-ink">
                                {model.display_name || model.model_id}
                              </span>
                            </td>
                            {showVendor ? (
                              <td className={`${bodyCell} text-muted`}>{model.vendor}</td>
                            ) : null}
                            {[
                              latest?.ttft_ms_p50,
                              latest?.ttft_ms_p95,
                              latest?.latency_ms_p50,
                              latest?.latency_ms_p95,
                            ].map((value, index) => (
                              <td key={index} className={numCell}>
                                {pending ? (
                                  <SkeletonValue />
                                ) : (
                                  formatLatencyMs(fresh ? (value ?? null) : null, locale)
                                )}
                              </td>
                            ))}
                            <td className={numCell}>
                              {pending ? (
                                <SkeletonValue />
                              ) : (
                                formatTps(fresh ? (latest?.tps ?? null) : null, locale)
                              )}
                            </td>
                            <td className={`${numCell} ${rateTone}`}>
                              {pending ? (
                                <SkeletonValue />
                              ) : (
                                formatSuccessRate(rate, locale)
                              )}
                            </td>
                            <td className={numCell}>
                              {pending ? (
                                <SkeletonValue />
                              ) : (
                                formatSampleSize(
                                  fresh ? (latest?.sample_size ?? null) : null,
                                  locale,
                                )
                              )}
                            </td>
                            <td className={`${numCell} text-muted`}>
                              {pending ? (
                                <SkeletonValue />
                              ) : fresh && latest?.tested_at ? (
                                <time dateTime={latest.tested_at}>
                                  {formatTestedAt(latest.tested_at, locale)}
                                </time>
                              ) : (
                                PLACEHOLDER
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
      </div>
    </section>
  );
}
