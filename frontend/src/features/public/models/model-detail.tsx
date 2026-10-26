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

import { formatMs } from './format'
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
  /**
   * 站点对外地址，由页面从 `/api/status` 读一次后下传。给了就不再自取：
   * 详情每展开一行都会挂载一次，`staleTime: 0` 下自取会每行多发一次
   * `/api/status`。单独渲染详情（测试、将来的详情页）时省略即可。
   */
  serverAddress?: string
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
 * P50 / P95 的唯一口径：两个都在才写 `x / y`，缺 P95 只写 `x`。
 * 每一侧都走表格的 `formatMs`，因此详情与表格对同一个读数给出同一个写法
 * （包括区域分组符号和 ≥10s 的秒制）。均值来源的 `p95` 恒为 null，因此
 * 永远不会出现拿均值冒充分位数的副读数。
 */
function formatPair(
  p50: number | null,
  p95: number | null,
  locale: string,
): string {
  if (p50 === null || !Number.isFinite(p50) || p50 < 0) return '—'
  const low = formatMs(p50, locale)
  if (p95 === null || !Number.isFinite(p95) || p95 < 0) return low
  return `${low} / ${formatMs(p95, locale)}`
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
  serverAddress,
}: ModelDetailProps) {
  const { t, locale } = useI18n()
  const detail = useQuery(pricingDetailQueryOptions(entry.card.model_id))
  const status = usePublicStatus(serverAddress === undefined)
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

  /**
   * 档案行按「有哪段写哪段」拼装：厂商为空时上下文与最大输出不能跟着消失。
   * 三段齐全时仍用完整句子，保持既有措辞。
   */
  const profileParts: string[] = []
  if (card.vendor !== '') profileParts.push(card.vendor)
  if (card.context_length !== null) {
    profileParts.push(t('public.pricing.context', { value: context }))
  }
  if (maxOutput !== null) {
    profileParts.push(`${t('public.models.detail.max_output')} ${output}`)
  }
  const profileLine =
    card.vendor !== '' && card.context_length !== null && maxOutput !== null
      ? t('public.models.detail.profile', {
          vendor: card.vendor,
          context,
          output,
        })
      : profileParts.join(' · ')

  // `p95` 缺失（均值来源）时标签必须点名这是 P50 单值，读者才知道那个数是
  // 什么；有 P95 才用组合标签。
  const hasP95 =
    perf.p95 !== null && Number.isFinite(perf.p95) && perf.p95 >= 0
  const latencyLabel = t(
    perf.measure === 'ttft'
      ? hasP95
        ? 'public.models.detail.ttft_p'
        : 'public.models.detail.ttft_p50'
      : hasP95
        ? 'public.models.detail.latency_p'
        : 'public.models.detail.latency_p50',
  )
  // 计数口径随来源：测速样本是探针次数，真实调用样本才是窗口内调用次数。
  const countsValue =
    perf.source === 'test'
      ? t('public.models.table.probe_runs', { count })
      : perf.source === 'usage'
        ? t('public.models.table.calls', { count })
        : t('public.models.table.evidence_none')

  const modalities =
    detail.data === undefined || detail.data.modalities.length === 0
      ? '—'
      : detail.data.modalities
          .map((modality) => modalityLabel(modality, t))
          .join(' · ')

  // base URL 只来自 `/api/status` 的 `site.server_address`（页面已读则直接
  // 用它）；未配置时退回相对路径并显式提示，绝不把当前页 origin 当成 API 地址。
  const target = sampleTarget(serverAddress ?? status.data?.site?.server_address)
  const spec = callSpec(entry.kind, card.model_id, false)
  const snippet = [
    `POST ${openaiBase(target.baseUrl)}${spec.path}`,
    `${t('public.models.detail.access_model')}  ${card.model_id}`,
    `${t('public.models.detail.access_auth')}   Bearer <your key>`,
  ].join('\n')

  /**
   * 按 token：金额 + 「/ M」；按次：金额本身就是元 / 次，不能再缀每百万
   * token 的单位。`perCall` 来自 `ModelPrice`，这里不重读 `card.mode`。
   */
  const priceText = (value: number | null, perMillion = true): string => {
    const formatted = formatRmb(value, locale)
    if (formatted === null) return '—'
    return perMillion
      ? `${formatted} ${t('public.models.detail.per_million')}`
      : formatted
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
                label={latencyLabel}
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
                label={t('public.models.table.evidence')}
                value={countsValue}
              />
            </dl>
            {perf.source === 'test' && perf.samples > 0 ? (
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
          {entry.price.perCall ? (
            <Kv
              label={t('public.pricing.price_per_call')}
              value={priceText(entry.price.input, false)}
            />
          ) : (
            <>
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
            </>
          )}
          <Kv
            label={t('public.models.detail.billing')}
            value={t(
              card.is_free
                ? 'public.models.detail.billing_free'
                : entry.price.perCall
                  ? 'public.pricing.mode_request'
                  : 'public.models.detail.billing_token',
            )}
          />
        </dl>
        {entry.price.perCall ? null : (
          <p className="text-badge leading-4 text-ink-3">
            {t('public.models.detail.price_note')}
          </p>
        )}
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
        {profileLine === '' ? null : (
          <p className="text-meta leading-5 text-ink-2">{profileLine}</p>
        )}

        <p className="mt-1 text-meta leading-5 text-ink-2">
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
