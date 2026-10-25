/**
 * 展开行的模型详情（规格表与卡片视图共用）。
 *
 * 静态档案（描述、上下文/输出、端点、能力）直接来自目录卡片，展开即可读；
 * 只有 `modalities` / `knowledge_cutoff` / `released_at` 这些卡片没有的字段
 * 才按需请求 `/api/pricing/{model_id}`。详情失败不撤掉已渲染的卡片字段，
 * 只在附加区给出重试，避免一次网络抖动让整行变空。
 */

import { useQuery } from '@tanstack/react-query'

import { pricingDetailQueryOptions } from '@/features/public/pricing/api'
import { formatContextTokens } from '@/features/public/pricing/format'
import {
  capabilityLabel,
  endpointLabel,
  modalityLabel,
} from '@/features/public/pricing/metadata'
import { useI18n } from '@/shared/lib/i18n-context'

import { formatSamples } from './format'
import type { ModelEntry } from './performance'

export interface ModelDetailProps {
  entry: ModelEntry
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

function SpecRow({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-badge text-ink-3">{label}</dt>
      <dd className="text-meta tabular-nums text-ink-1">{value}</dd>
    </div>
  )
}

export function ModelDetail({ entry }: ModelDetailProps) {
  const { t, locale } = useI18n()
  const detail = useQuery(pricingDetailQueryOptions(entry.card.model_id))
  const { card } = entry

  const displayName = card.display_name === '' ? card.model_id : card.display_name
  const context =
    card.context_length === null
      ? '—'
      : formatContextTokens(card.context_length, locale)
  const maxOutput = detail.data?.max_output ?? card.max_output
  const output =
    maxOutput === null ? '—' : formatContextTokens(maxOutput, locale)
  const modalities =
    detail.data === undefined
      ? '—'
      : detail.data.modalities.length === 0
        ? '—'
        : detail.data.modalities
            .map((modality) => modalityLabel(modality, t))
            .join(' · ')

  return (
    <section className="detail-grid" aria-label={displayName}>
      <div className="detail-block">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.description')}
        </h4>
        <p className="text-meta leading-5 text-ink-2">
          {card.description === '' ? '—' : card.description}
        </p>
        <h4 className="mt-2 text-badge font-medium text-ink-3">
          {t('public.models.table.capabilities')}
        </h4>
        <div className="cap-names flex flex-wrap gap-1">
          {card.capabilities.length === 0 ? (
            <span className="text-meta text-ink-2">—</span>
          ) : (
            card.capabilities.map((capability) => (
              <span
                className="cap-name border border-border-default px-1.5 text-badge text-ink-2"
                key={capability}
              >
                {capabilityLabel(capability, t)}
              </span>
            ))
          )}
        </div>
      </div>

      <div className="detail-block">
        <dl className="detail-list grid gap-1">
          <SpecRow label={t('public.models.table.context')} value={context} />
          <SpecRow label={t('public.models.detail.max_output')} value={output} />
          <SpecRow label={t('public.models.detail.modalities')} value={modalities} />
          <SpecRow
            label={t('public.models.detail.knowledge_cutoff')}
            value={formatMonth(detail.data?.knowledge_cutoff ?? null, locale)}
          />
          <SpecRow
            label={t('public.models.detail.released')}
            value={formatDay(detail.data?.released_at ?? null, locale)}
          />
        </dl>
      </div>

      <div className="detail-block">
        <h4 className="text-badge font-medium text-ink-3">
          {t('public.models.detail.endpoints')}
        </h4>
        {card.endpoints.length === 0 ? (
          <p className="text-meta text-ink-2">—</p>
        ) : (
          <ul className="grid gap-0.5">
            {card.endpoints.map((endpoint) => (
              <li className="text-meta text-ink-2" key={endpoint}>
                {endpointLabel(endpoint, t)}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="detail-block">
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
          <p
            className="text-meta tabular-nums text-ink-2"
            title={t('public.models.glossary.sample')}
          >
            <span className="mr-1 text-ink-3">
              {t(
                entry.perf.source === 'test'
                  ? 'public.models.detail.samples_test'
                  : 'public.models.detail.samples_usage',
              )}
            </span>
            {formatSamples(entry.perf.samples, locale)}
          </p>
        ) : null}
      </div>
    </section>
  )
}
