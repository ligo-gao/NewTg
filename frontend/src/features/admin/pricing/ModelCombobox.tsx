import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatTokenAmount } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'
import { Tag } from '@/shared/ui/tag'

import { isValidModelName } from './pricing-utils'

type PricingCandidate = components['schemas']['PricingCandidate']
type CandidateSource = PricingCandidate['sources'][number]

export interface ModelComboboxProps {
  value: string
  onChange: (model: string) => void
  /** 选中候选（用于把已定价行回填成草稿）；手动输入的模型不会触发。 */
  onSelectCandidate?: (candidate: PricingCandidate) => void
  disabled?: boolean
  error?: string
}

const MAX_SUGGESTIONS = 60

function sourceKey(source: CandidateSource): string {
  switch (source) {
    case 'catalog':
      return 'admin.pricing.model_source_catalog'
    case 'channel':
      return 'admin.pricing.model_source_channel'
    default:
      return 'admin.pricing.model_source_pricing'
  }
}

/**
 * 已添加模型的选择器（F-06-14）。数据来自 GET /api/model-pricing/candidates：
 * `model_meta` ∪ 渠道 supported_models ∪ model_mapping ∪ model_pricing 的并集。
 *
 * 输入框同时允许手打模型名（候选未覆盖的新模型），因此它既是搜索框也是
 * 模型名字段；候选列表只做建议，选中后回填已定价行或留作空白草稿。
 */
export function ModelCombobox({
  value,
  onChange,
  onSelectCandidate,
  disabled = false,
  error,
}: ModelComboboxProps) {
  const { t, locale } = useI18n()

  const candidatesQuery = useQuery({
    queryKey: ['admin-pricing-candidates'],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/model-pricing/candidates', {
          params: { query: { limit: 200 } },
        }),
      ),
    staleTime: 30_000,
    retry: false,
    enabled: !disabled,
  })

  const query = value.trim().toLowerCase()
  const suggestions = useMemo(() => {
    const items = candidatesQuery.data?.items ?? []
    const filtered =
      query === ''
        ? items
        : items.filter((candidate) => {
            const haystack = [
              candidate.model,
              candidate.display_name ?? '',
              candidate.vendor ?? '',
            ]
              .join(' ')
              .toLowerCase()
            return haystack.includes(query)
          })
    return filtered.slice(0, MAX_SUGGESTIONS)
  }, [candidatesQuery.data, query])

  return (    <div className="flex flex-col gap-3">
      <TextField
        label={t('admin.pricing.model')}
        value={value}
        disabled={disabled}
        maxLength={128}
        placeholder={t('admin.pricing.model_placeholder')}
        hint={disabled ? undefined : t('admin.pricing.model_hint')}
        error={error}
        autoComplete="off"
        onChange={(event) => {
          onChange(event.target.value)
        }}
      />

      {disabled ? null : (
        <section
          aria-label={t('admin.pricing.model_suggestions')}
          className="flex flex-col gap-1.5 rounded-lg border border-border-hairline bg-surface-subtle p-2"
        >
          <div className="flex items-center justify-between px-1">
            <span className="text-meta text-ink-2">
              {t('admin.pricing.model_suggestions')}
            </span>
            <span className="text-meta text-ink-3">
              {candidatesQuery.isSuccess
                ? t('admin.pricing.model_suggestions_count', {
                    count: candidatesQuery.data.items.length,
                  })
                : null}
            </span>
          </div>

          {candidatesQuery.isPending ? (
            <p className="px-1 py-2 text-meta text-ink-3" aria-busy="true">
              {t('admin.pricing.model_suggestions_loading')}
            </p>
          ) : candidatesQuery.isError ? (
            <div className="flex items-center gap-2 px-1 py-2 text-meta text-danger">
              <span>{t('admin.pricing.model_suggestions_error')}</span>
              <Button
                variant="ghost"
                className="h-6 px-1 text-meta"
                onClick={() => void candidatesQuery.refetch()}
              >
                {t('common.action.retry')}
              </Button>
            </div>
          ) : suggestions.length === 0 ? (
            <p className="px-1 py-2 text-meta text-ink-3">
              {query === ''
                ? t('admin.pricing.model_suggestions_empty')
                : t('admin.pricing.model_suggestions_no_match')}
            </p>
          ) : (
            <ul className="max-h-56 overflow-y-auto">
              {suggestions.map((candidate) => {
                const selected = candidate.model === value.trim()
                return (
                  <li key={candidate.model}>
                    <button
                      type="button"
                      aria-current={selected ? 'true' : undefined}
                      className={[
                        'flex w-full items-start justify-between gap-2 rounded-md px-2 py-1.5 text-left',
                        'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                        selected ? 'bg-surface-card' : 'hover:bg-surface-hover',
                      ].join(' ')}
                      onClick={() => {
                        onChange(candidate.model)
                        onSelectCandidate?.(candidate)
                      }}
                    >
                      <span className="flex min-w-0 flex-col">
                        <span className="truncate font-mono text-meta text-ink-1">
                          {candidate.model}
                        </span>
                        <span className="truncate text-meta text-ink-3">
                          {formatMetadata(
                            candidate,
                            locale,
                            t('admin.pricing.model_unknown_vendor'),
                            t('admin.pricing.model_context_unknown'),
                          )}
                        </span>
                      </span>
                      <span className="flex shrink-0 items-center gap-1">
                        {candidate.sources.map((source) => (
                          <Tag key={source} variant="neutral">
                            {t(sourceKey(source))}
                          </Tag>
                        ))}
                        <Tag
                          variant={candidate.priced ? 'success' : 'warning'}
                          ariaLabel={
                            candidate.priced
                              ? t('admin.pricing.model_priced')
                              : t('admin.pricing.model_unpriced')
                          }
                        >
                          {candidate.priced
                            ? t('admin.pricing.model_priced')
                            : t('admin.pricing.model_unpriced')}
                        </Tag>
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          )}

          {query !== '' && isValidModelName(query) ? (
            <p className="px-1 text-meta text-ink-3">
              {t('admin.pricing.model_suggestions_manual', { model: value.trim() })}
            </p>
          ) : null}
        </section>
      )}
    </div>
  )
}

function formatMetadata(
  candidate: PricingCandidate,
  locale: string,
  unknownVendor: string,
  unknownContext: string,
): string {
  const vendor = candidate.vendor ?? unknownVendor
  const context =
    candidate.context_length === null
      ? unknownContext
      : formatTokenAmount(candidate.context_length, locale)
  return `${vendor} · ${context}`
}
