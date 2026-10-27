import {
  Fragment,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
} from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerfModels } from '../api'
import { Sparkline, type SparklineTone } from '../charts/sparkline'
import {
  formatCount,
  formatDecimal,
  formatLatencyMs,
  formatPercent,
  perfStatusKey,
  perfStatusTone,
} from '../utils'
import { ModelDetail } from './model-detail'

/**
 * 模型性能表 + 单模型下钻（Task 16 / adjudications §3–4）。
 *
 * 行是**互斥手风琴**：同一时刻最多一行展开；展开行本身就是 `<tr>`，下钻内容
 * 放在紧随其后的 `<tr><td colSpan={10}>` 里。下钻组件只在展开时挂载，所以
 * 折叠态不会发出 `/api/model-perf` 请求（不预取）。
 *
 * 读数的空值口径：`null` 一律 `—`，绝不显示 0；`no_sample` 行的所有数字列
 * 都是 `—`（"没有样本"不是"样本为 0"）。
 */

type ModelRow = PerfModels['rows'][number]

/** 24h 分桶的 24 点趋势色：只有 degraded/incident 用状态色。 */
function sparkTone(status: string): SparklineTone | null {
  switch (status) {
    case 'degraded':
      return 'warning'
    case 'incident':
      return 'danger'
    case 'no_sample':
      return null
    default:
      return 'default'
  }
}

/** TPS 一律 1 位小数；null 不是 0。 */
function tpsText(value: number | null, locale: string): string {
  return value === null ? '—' : formatDecimal(value, locale)
}

/**
 * 模型列副标题。渠道数来自行上的 `channels`（渠道统计失败时为 null，此时
 * 不渲染副标题）；no_sample 行优先显示「尚无样本」，避免把渠道数误读成
 * 有流量。`meta_channels` 需要 provider，本分支没有这个数据源，已换成
 * 无 provider 的 `channels_count`。
 */
function modelSubtitle(row: ModelRow, t: TranslateFn): string | null {
  if (row.status === 'no_sample') {
    return t('admin.performance.models.no_sample')
  }
  if (row.channels === null) return null
  return t('admin.performance.models.channels_count', { count: row.channels })
}

export interface ModelPerfZoneProps {
  models: PerfModels
  /**
   * 监控（Prometheus）是否断线。模型表的数据来自指标库与中继流量，**不受**
   * Prometheus 影响（与体检条 banner「模型数据不受影响」同一口径），所以
   * 这个 prop 在本区只用于保持页面统一签名，不改变渲染。
   */
  sourceDown: boolean
  /** 「立即跑一轮」动作；当前分支没有该后端动作，页面不传就不渲染按钮。 */
  onRunNow?: () => void
}

export function ModelPerfZone({ models, onRunNow }: ModelPerfZoneProps) {
  const { t, locale } = useI18n()
  // 行互斥：单个字符串即「当前展开的模型」，null 表示全部折叠。
  const [openModel, setOpenModel] = useState<string | null>(null)

  function toggle(model: string): void {
    setOpenModel((current) => (current === model ? null : model))
  }

  function handleKeyDown(
    event: ReactKeyboardEvent<HTMLTableRowElement>,
    model: string,
  ): void {
    // Space 默认滚动页面，必须先阻止；Enter 对普通行没有默认行为。
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    toggle(model)
  }

  if (models.rows.length === 0) {
    return (
      <section aria-label={t('admin.performance.models.title')}>
        <div
          data-testid="models-empty"
          className="rounded-lg border border-dashed border-border-default bg-surface-subtle px-5 py-5"
        >
          <p className="font-semibold text-ink-1">
            {t('admin.performance.models.empty_title')}
          </p>
          <p className="mt-1.5 max-w-[72ch] text-meta text-ink-2">
            {t('admin.performance.models.empty_note')}
          </p>
          {onRunNow === undefined ? null : (
            <Button className="mt-3" onClick={onRunNow}>
              {t('admin.performance.models.empty_action')}
            </Button>
          )}
        </div>
      </section>
    )
  }

  return (
    <section aria-label={t('admin.performance.models.title')}>
      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        <table className="w-full min-w-[900px] text-body">
          <caption className="sr-only">
            {t('admin.performance.models.title')}
          </caption>
          <thead>
            <tr className="border-b border-border-hairline text-left text-badge text-ink-3">
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.models.col_model')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_ttft_p50')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_ttft_p95')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_latency_p95')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_success')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_uptime')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_tps')}
              </th>
              <th
                scope="col"
                className="px-4 py-2.5 text-right font-medium"
              >
                {t('admin.performance.models.col_samples')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.models.col_spark')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.models.col_status')}
              </th>
            </tr>
          </thead>
          <tbody>
            {models.rows.map((row) => {
              const open = row.model === openModel
              const noSample = row.status === 'no_sample'
              const dash = noSample ? '—' : null
              const tone = sparkTone(row.status)
              const subtitle = modelSubtitle(row, t)

              return (
                <Fragment key={row.model}>
                  <tr
                    data-testid="model-row"
                    data-model={row.model}
                    aria-expanded={open}
                    tabIndex={0}
                    onClick={() => toggle(row.model)}
                    onKeyDown={(event) => handleKeyDown(event, row.model)}
                    className={[
                      'cursor-pointer border-b border-border-hairline last:border-b-0 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                      open ? 'bg-surface-subtle' : '',
                    ]
                      .filter(Boolean)
                      .join(' ')}
                  >
                    <th scope="row" className="px-4 py-2.5 text-left font-normal">
                      <span className="block font-mono text-meta text-ink-1">
                        {row.model}
                      </span>
                      {subtitle === null ? null : (
                        <span className="mt-0.5 block text-badge text-ink-3">
                          {subtitle}
                        </span>
                      )}
                    </th>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ??
                        formatLatencyMs(row.ttft_p50_ms, row.ttft_p50_over_30s)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ??
                        formatLatencyMs(row.ttft_p95_ms, row.ttft_p95_over_30s)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ??
                        formatLatencyMs(
                          row.latency_p95_ms,
                          row.latency_p95_over_30s,
                        )}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ?? formatPercent(row.success_rate)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ?? formatPercent(row.uptime)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ?? tpsText(row.tps, locale)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-ink-1">
                      {dash ?? formatCount(row.samples, locale)}
                    </td>
                    <td className="px-4 py-2.5">
                      {tone === null ? null : (
                        <Sparkline
                          testId="model-spark"
                          values={row.spark}
                          tone={tone}
                        />
                      )}
                    </td>
                    <td className="px-4 py-2.5">
                      <StatusBadge variant={perfStatusTone(row.status)}>
                        {t(perfStatusKey(row.status))}
                      </StatusBadge>
                    </td>
                  </tr>
                  {open ? (
                    <tr data-testid="model-detail-row">
                      <td colSpan={10} className="px-4 py-4">
                        <ModelDetail model={row.model} />
                      </td>
                    </tr>
                  ) : null}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>

      <footer
        data-testid="models-footer"
        className="mt-2 space-y-1 text-badge text-ink-3"
      >
        <p>{t('admin.performance.models.foot_latency_source')}</p>
        <p>{t('admin.performance.models.foot_uptime_source')}</p>
      </footer>
    </section>
  )
}
