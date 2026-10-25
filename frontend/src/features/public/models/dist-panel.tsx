/**
 * 「当前筛选分布」面板（演示稿新增区块）。
 *
 * 统计的是**对话类模型的首字延迟读数中位数**：只有 `kind === 'chat'` 且
 * `measure === 'ttft'` 的条目才算首字读数。真实调用均值（`avg_ttft_ms`）
 * 是均值、不是分位数，但它的度量对象同样是首字延迟，因此保留在样本里；
 * `measure === 'latency'` 是端到端延迟，混进来会让「首字延迟」失去意义，
 * 必须排除。正因样本混合了测速 P50 与调用均值，文案描述的是「中位数」而
 * 不是「P50 分布」。
 *
 * 分桶与中位数复用 `format.ts` 的纯函数；没有样本时中位数是 `—`，绝不
 * 回退成 `0 ms`（0 是真实读数）。目录尚未就绪时整块不渲染，避免加载期间
 * 先闪一次「没有样本」的空态。
 */

import { useI18n } from '@/shared/lib/i18n-context'

import { formatMs, latencyHistogram, median } from './format'
import type { ModelsRange } from './params'
import type { ModelEntry } from './performance'

export interface DistPanelProps {
  entries: readonly ModelEntry[]
  range: ModelsRange
  /** 目录响应就绪后才展示；加载中/失败时由外层负责提示。 */
  ready?: boolean
}

/** 四个固定延迟桶的展示标签；边界值与 `latencyHistogram` 同口径。 */
const BIN_LABELS = ['<250', '250–499', '500–999', '≥1000'] as const

/** 空桶的基座高度：让用户看见「这一档没有模型」而不是柱子消失。 */
const EMPTY_BAR_PERCENT = 2

/** 有计数时柱子至少 7%，否则只有一个样本的档位几乎不可见。 */
const MIN_BAR_PERCENT = 7

export function DistPanel({ entries, range, ready = true }: DistPanelProps) {
  const { t, locale } = useI18n()
  if (!ready) return null

  const numberFormat = new Intl.NumberFormat(locale)
  const candidates: number[] = []
  for (const entry of entries) {
    if (entry.kind !== 'chat') continue
    if (entry.perf.measure !== 'ttft') continue
    const p50 = entry.perf.p50
    if (p50 === null || !Number.isFinite(p50)) continue
    candidates.push(p50)
  }

  const counts = latencyHistogram(candidates)
  const medianValue = median(candidates)
  const measured = candidates.length
  const maxCount = Math.max(...counts, 1)
  const empty = measured === 0
  const ariaLabel = empty
    ? t('public.models.dist.empty')
    : t('public.models.dist.aria', {
        count: numberFormat.format(measured),
        median: formatMs(medianValue),
      })

  return (
    <section className="border-t border-border-default pt-3">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="text-badge font-semibold text-ink-2">
            {t('public.models.dist.title', {
              range: t(`public.perf.range.${range}`),
            })}
          </h2>
          <p className="mt-0.5 text-badge text-ink-3">
            {t('public.models.dist.spread', {
              models: numberFormat.format(entries.length),
              measured: numberFormat.format(measured),
            })}
          </p>
        </div>
        <div className="flex shrink-0 items-baseline gap-1.5 whitespace-nowrap">
          <strong className="font-mono text-stat font-medium tabular-nums text-ink-1">
            {formatMs(medianValue)}
          </strong>
          <span className="text-badge text-ink-3">
            {t('public.models.dist.median')}
          </span>
        </div>
      </div>

      <div role="img" aria-label={ariaLabel} className="mt-3">
        {empty ? (
          <div className="flex h-12 items-center text-badge text-ink-3">
            {t('public.models.dist.empty')}
          </div>
        ) : (
          <>
            <div className="flex h-12 items-end gap-1.5 border-b border-border-default">
              {counts.map((count, index) => {
                const label = BIN_LABELS[index] ?? ''
                const height =
                  count === 0
                    ? EMPTY_BAR_PERCENT
                    : Math.max(
                        MIN_BAR_PERCENT,
                        Math.round((count / maxCount) * 100),
                      )
                return (
                  <div
                    key={label}
                    className="flex h-full flex-1 items-end"
                    title={t('public.models.dist.bin', {
                      label,
                      count: numberFormat.format(count),
                    })}
                  >
                    <span
                      className="w-full border border-brand-line border-t-2 border-t-brand bg-brand-subtle"
                      style={{ height: `${height}%` }}
                    />
                  </div>
                )
              })}
            </div>
            <div className="mt-1 flex gap-1.5">
              {BIN_LABELS.map((label) => (
                <span
                  key={label}
                  className="flex-1 text-center font-mono text-badge text-ink-3"
                >
                  {label}
                </span>
              ))}
            </div>
          </>
        )}
      </div>
    </section>
  )
}
