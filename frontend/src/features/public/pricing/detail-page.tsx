/**
 * 模型详情页（F-09-24 / F-09-27）。
 *
 * 信息区只展示后端实际返回的静态事实；`NULL`/空数组的行隐藏，而不是
 * 用 `0`、`—` 或编造值填充（R-PUB-01）。性能区按 P1 本波不渲染，也不
 * 请求 `/metrics`——后端已显式返回 404（not-implemented）。
 */

import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { Breadcrumb } from '@/features/public/breadcrumb'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { pricingDetailQueryOptions } from './api'
import { copyText } from './copy'
import { formatContextTokens, formatCredit, formatRatio } from './format'
import { IntegrationSection } from './integration-section'
import { PriceRegister } from './price-register'
import { DetailNotFound, InlineError } from './states'

export interface PricingDetailPageProps {
  modelId: string
}

function SpecItem({ label, value }: { label: string; value: string | null }) {
  if (value === null || value === '') return null
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs leading-[18px] text-ink-2">{label}</dt>
      <dd className="text-sm leading-[22px] text-ink-1">{value}</dd>
    </div>
  )
}

export function PricingDetailPage({ modelId }: PricingDetailPageProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const query = useQuery(pricingDetailQueryOptions(modelId))

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
            { label: t('public.nav.home'), path: '/' },
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
      value: detail.modalities.length === 0 ? null : detail.modalities.join(' · '),
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

  return (
    <div className="flex flex-col gap-6">
      <Breadcrumb
        items={[
          { label: t('public.nav.home'), path: '/' },
          { label: t('public.pricing.title'), path: '/pricing' },
          { label: detail.display_name, current: true },
        ]}
      />

      <div className="flex flex-col gap-2">
        <a
          href="/pricing"
          className="inline-flex items-center gap-1 self-start text-[13px] text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <span aria-hidden="true">←</span>
          {t('public.pricing.detail.back')}
        </a>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-xl font-semibold leading-7 text-ink-1">
            {detail.display_name}
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
        <p className="font-mono text-[13px] leading-5 text-ink-2">{detail.model_id}</p>
      </div>

      {specs.some((spec) => spec.value !== null && spec.value !== '') ? (
        <dl className="grid grid-cols-2 gap-4 rounded-xl border border-border-default bg-surface-card p-4 md:grid-cols-4">
          {specs.map((spec) => (
            <SpecItem key={spec.key} label={spec.label} value={spec.value} />
          ))}
        </dl>
      ) : null}

      <section aria-labelledby="pricing-detail-price" className="flex flex-col gap-2">
        <h2 id="pricing-detail-price" className="text-base font-semibold leading-6 text-ink-1">
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

      {detail.group_prices.length > 0 ? (
        <section aria-labelledby="pricing-detail-groups" className="flex flex-col gap-2">
          <h2 id="pricing-detail-groups" className="text-base font-semibold leading-6 text-ink-1">
            {t('public.pricing.detail.group_prices')}
          </h2>
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full min-w-[760px] text-[13px]">
              <thead className="bg-surface-subtle text-left text-xs text-ink-2">
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
            <div className="flex flex-wrap items-center gap-2 text-[13px] text-ink-2">
              <span className="font-medium text-ink-1">{t('public.pricing.detail.endpoints')}</span>
              {detail.endpoints.map((endpoint) => (
                <Tag key={endpoint} variant="neutral">{endpoint}</Tag>
              ))}
            </div>
          ) : null}
          {detail.tags.length > 0 ? (
            <div className="flex flex-wrap items-center gap-2 text-[13px] text-ink-2">
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
