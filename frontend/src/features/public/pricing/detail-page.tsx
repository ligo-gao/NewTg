/**
 * 模型详情页（F-09-24 / F-09-27）。
 *
 * 信息区只展示后端实际返回的静态事实；`NULL`/空数组的行隐藏，而不是
 * 用 `0`、`—` 或编造值填充（R-PUB-01）。
 *
 * 性能块（F-09-25）读取 `/api/pricing/{model_id}/metrics?range=7d`：优先
 * 展示 `usage_logs` 真实调用汇总，字段缺失时回退到最近一次主动测速
 * `latest_test`（独立兜底来源）。两者都缺失时显示 `—`；请求失败时保留
 * `—` 并在块内提供重试，不把整页变成错误态。
 */

import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { Breadcrumb } from '@/features/public/breadcrumb'
import { shellCopy } from '@/features/public/shell-copy'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { pricingDetailQueryOptions, pricingMetricsQueryOptions } from './api'
import { copyText } from './copy'
import {
  formatContextTokens,
  formatCredit,
  formatPerfMilliseconds,
  formatPerfTPS,
  formatRatio,
  formatSuccessRate,
} from './format'
import { IntegrationSection } from './integration-section'
import { capabilityLabel, endpointLabel, modalityLabel } from './metadata'
import { PriceRegister } from './price-register'
import { DetailNotFound, InlineError } from './states'

export interface PricingDetailPageProps {
  modelId: string
}

function SpecItem({ label, value }: { label: string; value: string | null }) {
  if (value === null || value === '') return null
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-meta leading-[18px] text-ink-2">{label}</dt>
      <dd className="text-body leading-[22px] text-ink-1">{value}</dd>
    </div>
  )
}

export function PricingDetailPage({ modelId }: PricingDetailPageProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const query = useQuery(pricingDetailQueryOptions(modelId))
  // 模型不存在时详情已走 404 分支；指标端点对同一路径同样 404，等详情
  // 成功再请求可以少一次无意义的错误请求。
  const metricsQuery = useQuery({
    ...pricingMetricsQueryOptions(modelId),
    enabled: query.isSuccess,
  })

  const copyName = async () => {
    const ok = await copyText(modelId)
    push({
      tone: ok ? 'success' : 'error',
      message: ok ? t('public.pricing.copied') : t('public.pricing.copy_failed'),
    })
  }

  if (query.isError && query.error instanceof ApiError && query.error.kind === 'not_found') {
    return (
      <div className="flex flex-col gap-4">
        <Breadcrumb
          items={[
            { label: shellCopy(locale)['nav.home'], path: '/' },
            { label: t('public.pricing.title'), path: '/pricing' },
            { label: modelId, current: true },
          ]}
        />
        <DetailNotFound />
      </div>
    )
  }

  if (query.isPending) {
    return (
      <div aria-busy="true" className="flex flex-col gap-6">
        <span className="sr-only">{t('public.pricing.loading')}</span>
        <div className="h-8 w-1/3 rounded bg-surface-subtle" />
        <div className="grid grid-cols-2 gap-4 rounded-xl border border-border-hairline bg-surface-card p-4 md:grid-cols-4">
          {[0, 1, 2, 3, 4, 5, 6, 7].map((item) => (
            <div key={item} className="h-10 rounded bg-surface-subtle" />
          ))}
        </div>
        <div className="h-36 rounded-xl border border-border-hairline bg-surface-card" />
      </div>
    )
  }

  if (query.isError) {
    return (
      <InlineError
        message={
          query.error instanceof ApiError && query.error.kind === 'offline'
            ? t('public.pricing.offline')
            : query.error instanceof ApiError && query.error.kind === 'rate_limited'
              ? t('public.pricing.rate_limited')
              : t('public.pricing.error')
        }
        onRetry={() => void query.refetch()}
      />
    )
  }

  const detail = query.data
  const displayName = detail.display_name === '' ? detail.model_id : detail.display_name
  const specs = [
    {
      key: 'context',
      label: t('public.pricing.detail.spec_context'),
      value:
        detail.context_length === null
          ? null
          : formatContextTokens(detail.context_length, locale),
    },
    {
      key: 'max_output',
      label: t('public.pricing.detail.spec_max_output'),
      value:
        detail.max_output === null ? null : formatContextTokens(detail.max_output, locale),
    },
    {
      key: 'modalities',
      label: t('public.pricing.detail.spec_modalities'),
      value:
        detail.modalities.length === 0
          ? null
          : detail.modalities
              .map((modality) => modalityLabel(modality, t))
              .join(' · '),
    },
    {
      key: 'knowledge_cutoff',
      label: t('public.pricing.detail.spec_knowledge_cutoff'),
      value: detail.knowledge_cutoff,
    },
    {
      key: 'released_at',
      label: t('public.pricing.detail.spec_released'),
      value: detail.released_at,
    },
  ]

  // 真实调用汇总优先；某个字段缺失（如无流式样本）时回退到主动测速，
  // 两者都没有就是 `—`（不把「没有样本」渲染成 0）。
  const metricsSummary = metricsQuery.data?.summary
  const latestTest = metricsQuery.data?.latest_test ?? null
  const performanceStats = [
    {
      key: 'ttft',
      label: t('public.perf.avg_ttft'),
      value: formatPerfMilliseconds(
        metricsSummary?.avg_ttft_ms ?? latestTest?.ttft_ms_p50 ?? null,
        locale,
      ),
    },
    {
      key: 'latency',
      label: t('public.perf.avg_latency'),
      value: formatPerfMilliseconds(
        metricsSummary?.avg_latency_ms ?? latestTest?.latency_ms_p50 ?? null,
        locale,
      ),
    },
    {
      key: 'tps',
      label: t('public.perf.tps'),
      value: formatPerfTPS(
        metricsSummary?.tps ?? latestTest?.tps ?? null,
        locale,
      ),
    },
    {
      key: 'success_rate',
      label: t('public.perf.success_rate'),
      value: formatSuccessRate(
        metricsSummary?.success_rate ?? latestTest?.success_rate ?? null,
        locale,
      ),
    },
  ]

  return (
    <div className="flex flex-col gap-6">
      <Breadcrumb
        items={[
          { label: shellCopy(locale)['nav.home'], path: '/' },
          { label: t('public.pricing.title'), path: '/pricing' },
          { label: displayName, current: true },
        ]}
      />

      <div className="flex flex-col gap-2">
        <a
          href="/pricing"
          className="inline-flex items-center gap-1 self-start text-meta text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <span aria-hidden="true">←</span>
          {t('public.pricing.detail.back')}
        </a>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-public-title font-semibold leading-7 text-ink-1">
            {displayName}
          </h1>
          <button
            type="button"
            aria-label={t('public.pricing.copy_model', { name: detail.model_id })}
            onClick={() => void copyName()}
            className="inline-flex size-7 items-center justify-center rounded text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
              <rect x="5" y="5" width="8" height="8" rx="1.5" />
              <path d="M11 5V4a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h1" />
            </svg>
          </button>
          {detail.vendor !== '' ? <Tag variant="neutral">{detail.vendor}</Tag> : null}
          <Tag variant={detail.mode === 'per_request' ? 'neutral' : 'info'}>
            {detail.mode === 'per_request'
              ? t('public.pricing.mode_request')
              : t('public.pricing.mode_token')}
          </Tag>
          {detail.is_free ? (
            <Tag variant="success">{t('public.pricing.free')}</Tag>
          ) : null}
        </div>
        <p className="font-mono text-meta leading-5 text-ink-2">{detail.model_id}</p>
        {detail.capabilities.length > 0 ? (
          <div className="flex flex-wrap gap-1">
            {detail.capabilities.map((capability) => (
              <Tag key={capability} variant="neutral">
                {capabilityLabel(capability, t)}
              </Tag>
            ))}
          </div>
        ) : null}
      </div>

      {specs.some((spec) => spec.value !== null && spec.value !== '') ? (
        <dl className="grid grid-cols-2 gap-4 rounded-xl border border-border-default bg-surface-card p-4 md:grid-cols-4">
          {specs.map((spec) => (
            <SpecItem key={spec.key} label={spec.label} value={spec.value} />
          ))}
        </dl>
      ) : null}

      <section aria-labelledby="pricing-detail-price" className="flex flex-col gap-2">
        <h2 id="pricing-detail-price" className="text-section font-semibold leading-6 text-ink-1">
          {t('public.pricing.detail.price_title')}
        </h2>
        <div className="rounded-xl border border-border-default bg-surface-card p-4">
          <PriceRegister
            card={detail}
            meta={undefined}
            tokenUnit="M"
            dualCurrency={false}
            rechargePrice={false}
          />
        </div>
      </section>

      <section aria-labelledby="pricing-detail-performance" className="flex flex-col gap-2">
        <h2 id="pricing-detail-performance" className="text-section font-semibold leading-6 text-ink-1">
          {t('public.perf.title')}
        </h2>
        <div className="rounded-xl border border-border-default bg-surface-card p-4">
          <dl className="grid grid-cols-2 gap-4 md:grid-cols-4">
            {performanceStats.map((stat) => (
              <div key={stat.key} className="flex flex-col gap-0.5">
                <dt className="text-meta leading-[18px] text-ink-2">{stat.label}</dt>
                <dd className="text-body leading-[22px] tabular-nums text-ink-1">
                  {stat.value}
                </dd>
              </div>
            ))}
          </dl>
          {metricsQuery.isError ? (
            <div className="mt-3">
              <InlineError
                message={
                  metricsQuery.error instanceof ApiError &&
                  metricsQuery.error.kind === 'rate_limited'
                    ? t('public.perf.rate_limited')
                    : t('public.perf.unavailable')
                }
                onRetry={() => void metricsQuery.refetch()}
              />
            </div>
          ) : null}
        </div>
      </section>

      {detail.group_prices.length > 0 ? (
        <section aria-labelledby="pricing-detail-groups" className="flex flex-col gap-2">
          <h2 id="pricing-detail-groups" className="text-section font-semibold leading-6 text-ink-1">
            {t('public.pricing.detail.group_prices')}
          </h2>
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full min-w-[760px] text-meta">
              <thead className="bg-surface-subtle text-left text-meta text-ink-2">
                <tr className="h-9">
                  <th scope="col" className="px-3">{t('public.pricing.detail.group')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.detail.ratio')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.price_input')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.price_output')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.price_cache_read')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.price_cache_write')}</th>
                  <th scope="col" className="px-3">{t('public.pricing.detail.source')}</th>
                </tr>
              </thead>
              <tbody>
                {detail.group_prices.map((price) => {
                  return (
                    <tr
                      key={price.group}
                      className={[
                        'h-11 border-t border-border-hairline',
                      ].join(' ')}
                    >
                      <td className="px-3 text-ink-1">
                        {price.group}
                      </td>
                      <td className="px-3 text-right font-mono tabular-nums text-ink-2">
                        {formatRatio(price.ratio) ?? '—'}
                      </td>
                      <td className="px-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCredit(price.prompt, locale)}
                      </td>
                      <td className="px-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCredit(price.completion, locale)}
                      </td>
                      <td className="px-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCredit(price.cache_read, locale)}
                      </td>
                      <td className="px-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCredit(price.cache_write, locale)}
                      </td>
                      <td className="px-3 text-ink-2">
                        {price.source === 'override'
                          ? t('public.pricing.detail.source_override')
                          : t('public.pricing.detail.source_ratio')}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}

      {detail.endpoints.length > 0 || detail.tags.length > 0 ? (
        <section className="flex flex-col gap-2">
          {detail.endpoints.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2 text-meta text-ink-2">
              <span className="font-medium text-ink-1">{t('public.pricing.detail.endpoints')}</span>
              {detail.endpoints.map((endpoint) => (
                <Tag key={endpoint} variant="neutral">{endpointLabel(endpoint, t)}</Tag>
              ))}
            </div>
          ) : null}
          {detail.tags.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2 text-meta text-ink-2">
              <span className="font-medium text-ink-1">{t('public.pricing.detail.tags')}</span>
              {detail.tags.map((tag) => (
                <Tag key={tag} variant="neutral">{tag}</Tag>
              ))}
            </div>
          ) : null}
        </section>
      ) : null}

      <IntegrationSection
        detail={detail}
        serverAddress={status.data?.site?.server_address}
        isLoggedIn={me.isSuccess}
      />
    </div>
  )
}
