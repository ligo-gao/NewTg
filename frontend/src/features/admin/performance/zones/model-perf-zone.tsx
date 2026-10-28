import {
  Fragment,
  useId,
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
import { ZoneRail } from './primitives'

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
   * 测速节奏（来自运行参数的 schedule）。只在调度器已启用且间隔合法时由
   * 页面传入；缺席时不渲染 rail 的节奏说明，不臆造默认值。
   */
  probeSchedule?: { hours: number; runs: number }
  /** 「立即跑一轮」动作；走既有的任务触发端点，由页面装配并传入。 */
  onRunNow?: () => void
}

/** 某数据源是否可用；缺该条按不可用处理（不臆造数据源）。 */
function sourceOk(models: PerfModels, name: 'perfmetrics' | 'probes'): boolean {
  return models.sources.find((source) => source.name === name)?.status === 'ok'
}

interface EmptyCopy {
  titleKey: string
  noteKey: string
}

/**
 * 空表时的文案分流：指标库不可用和测速结果不可用是两种不同的结论，
 * 不能一律说「还没有采样数据」。指标库优先——表格的行本身来自它。
 */
function emptyCopy(models: PerfModels): EmptyCopy {
  if (!sourceOk(models, 'perfmetrics')) {
    return {
      titleKey: 'admin.performance.models.empty_source_perfmetrics_title',
      noteKey: 'admin.performance.models.empty_source_perfmetrics_note',
    }
  }
  if (!sourceOk(models, 'probes')) {
    return {
      titleKey: 'admin.performance.models.empty_source_probes_title',
      noteKey: 'admin.performance.models.empty_source_probes_note',
    }
  }
  return {
    titleKey: 'admin.performance.models.empty_title',
    noteKey: 'admin.performance.models.empty_note',
  }
}

export function ModelPerfZone({
  models,
  probeSchedule,
  onRunNow,
}: ModelPerfZoneProps) {
  const { t, locale } = useI18n()
  const titleId = useId()
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

  const rail = (
    <ZoneRail
      titleId={titleId}
      title={t('admin.performance.models.title')}
      sub={t('admin.performance.models.rail_sub', {
        count: models.rows.length,
      })}
      note={
        probeSchedule === undefined
          ? undefined
          : t('admin.performance.models.rail_note', {
              hours: probeSchedule.hours,
              runs: probeSchedule.runs,
            })
      }
    />
  )

  if (models.rows.length === 0) {
    const copy = emptyCopy(models)
    return (
      <section aria-labelledby={titleId}>
        {rail}
        <div
          data-testid="models-empty"
          className="rounded-lg border border-dashed border-border-default bg-surface-subtle px-5 py-5"
        >
          <p className="font-semibold text-ink-1">{t(copy.titleKey)}</p>
          <p className="mt-1.5 max-w-[72ch] text-meta text-ink-2">
            {t(copy.noteKey)}
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
    <section aria-labelledby={titleId}>
      {rail}
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
              // 编码后放进 id：模型名可能含 `/`、`&`、`=` 等字符。
              const detailId = `model-detail-${encodeURIComponent(row.model)}`

              return (
                <Fragment key={row.model}>
                  <tr
                    data-testid="model-row"
                    data-model={row.model}
                    aria-expanded={open}
                    aria-controls={open ? detailId : undefined}
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
                      {dash ?? formatPercent(row.uptime, 2)}
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
                    <tr data-testid="model-detail-row" id={detailId}>
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
