/**
 * 展开行的模型详情（规格表与卡片视图共用）。
 *
 * 四块自上而下：实测读数（窗口口径 + 探针说明）、价格、接入（端点 / 复制 /
 * 对比 / 调用示例）、说明（口径 + 档案 + 目录卡片字段 + 详情三态）。读数
 * 只展示 `ModelPerformance` 实际携带的那一种口径（首字或端到端），均值来
 * 源没有 P95 时绝不补造；价格缺一项就写 `—`，不冒充免费。
 *
 * 静态档案（描述、厂商、上下文、最大输出）直接来自目录卡片，展开即可读；
 * 只有 `modalities` / `knowledge_cutoff` / `released_at` 这些卡片没有的字段
 * 才按需请求 `/api/pricing/{model_id}`。详情失败不撤掉已渲染的卡片字段，
 * 只在说明块给出重试，避免一次网络抖动让整行变空。
 */

import { useQuery } from '@tanstack/react-query'

import { usePublicStatus } from '@/lib/public-status'
import { pricingDetailQueryOptions } from '@/features/public/pricing/api'
import { openaiBase, sampleTarget } from '@/features/public/pricing/code-samples'
import { copyText } from '@/features/public/pricing/copy'
import { formatContextTokens } from '@/features/public/pricing/format'
import { modalityLabel } from '@/features/public/pricing/metadata'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

import { MAX_COMPARE, type ModelsRange } from './params'
import type { ModelEntry } from './performance'
import { formatRmb } from './price'
import { callSpec } from './samples'

export interface ModelDetailProps {
  entry: ModelEntry
  /** 指标窗口；决定读数块标题里的「近 24 小时 / 近 7 天」。 */
  range: ModelsRange
  onCall: (entry: ModelEntry) => void
  onToggleCompare: (modelId: string) => void
  /** 当前模型是否已在对比集合里。 */
  compared: boolean
  /** 对比集合已满且当前模型未被选中：按钮禁用并提示上限。 */
  compareFull: boolean
}

const monthFormat = (locale: string) =>
  new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: 'short',
    timeZone: 'UTC',
  })

const dayFormat = (locale: string) =>
  new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeZone: 'UTC',
  })

/** `YYYY-MM` → 本地化月份；无法解析时原样显示，不编造日期。 */
function formatMonth(value: string | null, locale: string): string {
  if (value === null || value === '') return '—'
  const match = /^(\d{4})-(\d{2})$/.exec(value)
  if (match === null) return value
  const year = Number(match[1])
  const month = Number(match[2])
  return monthFormat(locale).format(new Date(Date.UTC(year, month - 1, 1)))
}

/** `YYYY-MM-DD` → 本地化日期；无法解析时原样显示。 */
function formatDay(value: string | null, locale: string): string {
  if (value === null || value === '') return '—'
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  if (match === null) return value
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  return dayFormat(locale).format(new Date(Date.UTC(year, month - 1, day)))
}

/**
 * P50 / P95 的唯一口径：两个都在才写 `x / y ms`，缺 P95 只写 `x ms`。
 * 均值来源的 `p95` 恒为 null，因此永远不会出现拿均值冒充分位数的副读数。
 */
function formatPair(
  p50: number | null,
  p95: number | null,
  locale: string,
): string {
  if (p50 === null || !Number.isFinite(p50) || p50 < 0) return '—'
  const nf = new Intl.NumberFormat(locale, { maximumFractionDigits: 0 })
  const low = nf.format(Math.round(p50))
  if (p95 === null || !Number.isFinite(p95) || p95 < 0) return `${low} ms`
  return `${low} / ${nf.format(Math.round(p95))} ms`
}

/** 成功率固定两位小数；没有读数写 `—`，绝不写 `0%`。 */
function percentValue(rate: number | null): string {
  if (rate === null || !Number.isFinite(rate)) return '—'
  return `${(rate * 100).toFixed(2)}%`
}

/** 输出吞吐固定一位小数；缺失或非法值写 `—`（与表格的吞吐格同口径）。 */
function tpsValue(tps: number | null): string {
  if (tps === null || !Number.isFinite(tps) || tps < 0) return '—'
  return `${tps.toFixed(1)} t/s`
}

function Kv({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-badge text-ink-3">{label}</dt>
      <dd className="font-mono text-meta tabular-nums text-ink-1">{value}</dd>
    </div>
  )
}

const ACTION_CLASS =
  'inline-flex min-h-7 items-center justify-center border border-border-default px-2 text-badge leading-none text-ink-2 hover:border-brand hover:bg-brand-subtle hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:border-border-default disabled:hover:bg-transparent disabled:hover:text-ink-2'

export function ModelDetail({
  entry,
  range,
  onCall,
  onToggleCompare,
  compared,
  compareFull,
}: ModelDetailProps) {
  const { t, locale } = useI18n()
  const detail = useQuery(pricingDetailQueryOptions(entry.card.model_id))
  const status = usePublicStatus()
  const push = useToastStore((state) => state.push)
  const { card, perf } = entry

  const displayName = card.display_name === '' ? card.model_id : card.display_name
  const windowLabel = t(
    range === '24h'
      ? 'public.models.detail.window_24h'
      : 'public.models.detail.window_7d',
  )
  const count = new Intl.NumberFormat(locale).format(perf.samples)

  const context =
    card.context_length === null
      ? '—'
      : formatContextTokens(card.context_length, locale)
  const maxOutput = detail.data?.max_output ?? card.max_output
  const output =
    maxOutput === null ? '—' : formatContextTokens(maxOutput, locale)
  const modalities =
    detail.data === undefined || detail.data.modalities.length === 0
      ? '—'
      : detail.data.modalities
          .map((modality) => modalityLabel(modality, t))
          .join(' · ')

  // base URL 只来自 `/api/status` 的 `site.server_address`；未配置时退回相对
  // 路径并显式提示，绝不把当前页 origin 当成 API 地址。
  const target = sampleTarget(status.data?.site?.server_address)
  const spec = callSpec(entry.kind, card.model_id, false)
  const snippet = [
    `POST ${openaiBase(target.baseUrl)}${spec.path}`,
    `${t('public.models.detail.access_model')}  ${card.model_id}`,
    `${t('public.models.detail.access_auth')}   Bearer <your key>`,
  ].join('\n')

  const priceText = (value: number | null): string => {
    const formatted = formatRmb(value, locale)
    return formatted === null
      ? '—'
      : `${formatted} ${t('public.models.detail.per_million')}`
  }

  const copyModelId = async () => {
    const ok = await copyText(card.model_id)
    push({
      tone: ok ? 'success' : 'error',
      message: ok
        ? t('public.models.call.copied')
        : t('public.pricing.copy_failed'),
    })
  }

  return (
    <section
      aria-label={displayName}
      className="grid gap-5 px-4 py-4 md:grid-cols-2 xl:grid-cols-4"
    >
      <div className="flex min-w-0 flex-col gap-1">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.window', { window: windowLabel })}
        </h4>
        {perf.hasData ? (
          <>
            <dl className="grid gap-1">
              <Kv
                label={t(
                  perf.measure === 'ttft'
                    ? 'public.models.detail.ttft_p'
                    : 'public.models.detail.latency_p',
                )}
                value={formatPair(perf.p50, perf.p95, locale)}
              />
              <Kv
                label={t('public.models.detail.tps')}
                value={tpsValue(perf.tps)}
              />
              <Kv
                label={t('public.models.detail.success')}
                value={percentValue(perf.successRate)}
              />
              <Kv
                label={t('public.models.detail.calls')}
                value={t('public.models.detail.calls_value', { count })}
              />
            </dl>
            {perf.samples > 0 ? (
              <p className="text-badge leading-4 text-ink-3">
                {t('public.models.detail.probe_note', { count })}
              </p>
            ) : null}
          </>
        ) : (
          <p className="text-meta text-ink-2">
            {t('public.models.detail.no_samples')}
          </p>
        )}
      </div>

      <div className="flex min-w-0 flex-col gap-1">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.block_price')}
        </h4>
        <dl className="grid gap-1">
          <Kv
            label={t('public.models.detail.price_input')}
            value={priceText(entry.price.input)}
          />
          <Kv
            label={t('public.models.detail.price_output')}
            value={priceText(entry.price.output)}
          />
          <Kv
            label={t('public.models.detail.price_cache')}
            value={priceText(entry.price.cacheRead)}
          />
          <Kv
            label={t('public.models.detail.billing')}
            value={t(
              card.is_free
                ? 'public.models.detail.billing_free'
                : 'public.models.detail.billing_token',
            )}
          />
        </dl>
        <p className="text-badge leading-4 text-ink-3">
          {t('public.models.detail.price_note')}
        </p>
      </div>

      <div className="flex min-w-0 flex-col gap-1">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.block_access')}
        </h4>
        <pre className="overflow-x-auto border border-border-default bg-surface-subtle p-2 text-badge leading-5 text-ink-1">
          <code>{snippet}</code>
        </pre>
        {target.configured ? null : (
          <p className="text-badge leading-4 text-warning">
            {t('public.models.call.server_unset')}
          </p>
        )}
        <div className="mt-1 flex flex-wrap gap-1.5">
          <button
            type="button"
            className={ACTION_CLASS}
            onClick={() => void copyModelId()}
          >
            {t('public.models.detail.copy_model_id')}
          </button>
          <button
            type="button"
            className={ACTION_CLASS}
            aria-pressed={compared}
            disabled={!compared && compareFull}
            title={
              !compared && compareFull
                ? t('public.models.register.compare_full', { max: MAX_COMPARE })
                : undefined
            }
            onClick={() => onToggleCompare(card.model_id)}
          >
            {t('public.models.table.compare')}
          </button>
          <button
            type="button"
            className={ACTION_CLASS}
            onClick={() => onCall(entry)}
          >
            {t('public.models.detail.call_examples')}
          </button>
        </div>
      </div>

      <div className="flex min-w-0 flex-col gap-1">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.block_notes')}
        </h4>
        <p className="text-meta leading-5 text-ink-2">
          {t('public.models.detail.notes_window', { window: windowLabel })}
        </p>
        {card.vendor === '' ? null : (
          <p className="text-meta leading-5 text-ink-2">
            {t('public.models.detail.profile', {
              vendor: card.vendor,
              context,
              output,
            })}
          </p>
        )}

        <h5 className="mt-1 text-badge font-medium text-ink-3">
          {t('public.models.detail.description')}
        </h5>
        <p className="text-meta leading-5 text-ink-2">
          {card.description === '' ? '—' : card.description}
        </p>

        {detail.isPending ? (
          <p
            className="text-meta text-ink-3"
            role="status"
            aria-label={t('public.models.detail.loading')}
          >
            {t('public.models.detail.loading')}
          </p>
        ) : null}

        {detail.isError ? (
          <div className="grid gap-1">
            <p className="text-meta text-ink-2" role="alert">
              {t('public.models.detail.failed')}
            </p>
            <button
              type="button"
              onClick={() => void detail.refetch()}
              className="w-fit border border-border-default px-2 text-badge text-ink-2 hover:border-brand hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.models.detail.retry')}
            </button>
          </div>
        ) : null}

        {detail.isSuccess ? (
          <dl className="grid gap-1">
            <Kv
              label={t('public.models.detail.modalities')}
              value={modalities}
            />
            <Kv
              label={t('public.models.detail.knowledge_cutoff')}
              value={formatMonth(detail.data.knowledge_cutoff, locale)}
            />
            <Kv
              label={t('public.models.detail.released')}
              value={formatDay(detail.data.released_at, locale)}
            />
          </dl>
        ) : null}
      </div>
    </section>
  )
}
