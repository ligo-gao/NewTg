/**
 * 模型卡片（F-09-18）。设计口径见 modules-09 UI 子模块 B §5：
 * 1px 描边、无阴影、价签区上下发丝线，层级只靠对齐与一条 brand 竖线。
 */

import type { PublicModelCard, PublicPricingMeta } from './api'
import { copyText } from './copy'
import { formatContextTokens } from './format'
import { HighlightedText } from './highlight'
import { capabilityLabel, endpointLabel } from './metadata'
import type { PricingTokenUnit } from './params'
import { PriceRegister } from './price-register'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'
import { Tag } from '@/shared/ui/tag'

export interface ModelCardProps {
  card: PublicModelCard
  meta?: PublicPricingMeta
  tokenUnit: PricingTokenUnit
  dualCurrency: boolean
  rechargePrice: boolean
  search: string
  isOwnGroup: boolean
}

function detailHref(modelId: string): string {
  return `/pricing/${encodeURIComponent(modelId)}`
}

export function ModelCard({
  card,
  meta,
  tokenUnit,
  dualCurrency,
  rechargePrice,
  search,
  isOwnGroup,
}: ModelCardProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)

  const copy = async () => {
    const ok = await copyText(card.model_id)
    push({
      tone: ok ? 'success' : 'error',
      message: ok ? t('public.pricing.copied') : t('public.pricing.copy_failed'),
    })
  }

  const context =
    card.context_length === null
      ? null
      : formatContextTokens(card.context_length, locale)

  // 契约保证 display_name 空串时后端已回退 model_id；这里再兜一层，防止
  // 老缓存/第三方实现漏回退时标题区变成空白（无档案卡片必须保持可识别）。
  const displayName = card.display_name === '' ? card.model_id : card.display_name

  return (
    <article className="flex flex-col rounded-xl border border-border-default bg-surface-card p-4 transition-colors duration-150 hover:bg-surface-hover motion-reduce:transition-none">
      <div className="flex items-start justify-between gap-2">
        <h3 className="min-w-0 text-[15px] font-semibold leading-6 text-ink-1">
          <a href={detailHref(card.model_id)} className="hover:underline">
            <HighlightedText text={displayName} query={search} />
          </a>
        </h3>
        <div className="flex shrink-0 items-center gap-1">
          <Tag variant={card.mode === 'per_request' ? 'neutral' : 'info'}>
            {card.mode === 'per_request'
              ? t('public.pricing.mode_request')
              : t('public.pricing.mode_token')}
          </Tag>
          {card.is_free ? (
            <Tag variant="success">{t('public.pricing.free')}</Tag>
          ) : null}
        </div>
      </div>

      <div className="mt-0.5 flex min-w-0 items-center gap-1 text-xs leading-5 text-ink-2">
        <span className="truncate font-mono">{card.model_id}</span>
        {card.vendor !== '' ? (
          <>
            <span aria-hidden="true">·</span>
            <span className="shrink-0 truncate">{card.vendor}</span>
          </>
        ) : null}
        <button
          type="button"
          onClick={() => void copy()}
          aria-label={t('public.pricing.copy_model', { name: card.model_id })}
          className="inline-flex size-5 shrink-0 items-center justify-center rounded text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-3.5" fill="none" stroke="currentColor" strokeWidth="1.5">
            <rect x="5" y="5" width="8" height="8" rx="1.5" />
            <path d="M11 5V4a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h1" />
          </svg>
        </button>
      </div>

      {context !== null || card.max_output !== null ? (
        <p className="mt-1 text-[13px] leading-5 tabular-nums text-ink-2">
          {context === null
            ? t('public.pricing.context_unknown')
            : t('public.pricing.context', { value: context })}
          {card.max_output !== null
            ? ` · ${t('public.pricing.max_output', {
                value: formatContextTokens(card.max_output, locale),
              })}`
            : null}
        </p>
      ) : null}

      <div className="mt-2">
        <PriceRegister
          card={card}
          meta={meta}
          tokenUnit={tokenUnit}
          dualCurrency={dualCurrency}
          rechargePrice={rechargePrice}
          isOwnGroup={isOwnGroup}
        />
      </div>

      {card.capabilities.length > 0 ? (
        <div className="mt-2 flex flex-wrap gap-1">
          {card.capabilities.map((capability) => (
            <Tag key={capability} variant="neutral">
              {capabilityLabel(capability, t)}
            </Tag>
          ))}
        </div>
      ) : null}

      {/*
       * tags/endpoints 只来自模型档案：无档案时恒为空数组，因此整行不渲染，
       * 卡片保持与元数据落库前完全一致的降级外观（R-PUB-01）。
       */}
      {card.tags.length > 0 || card.endpoints.length > 0 ? (
        <div className="mt-1.5 flex flex-wrap gap-1">
          {card.tags.map((tag) => (
            <Tag key={`tag-${tag}`} variant="neutral">
              {tag}
            </Tag>
          ))}
          {card.endpoints.map((endpoint) => (
            <Tag key={`endpoint-${endpoint}`} variant="info">
              {endpointLabel(endpoint, t)}
            </Tag>
          ))}
        </div>
      ) : null}

      {card.description !== '' ? (
        <p className="mt-2 line-clamp-2 text-[13px] leading-5 text-ink-2">
          {card.description}
        </p>
      ) : null}

      <div className="mt-3 flex justify-end">
        <a
          href={detailHref(card.model_id)}
          className="inline-flex items-center gap-1 text-[13px] font-medium text-brand hover:text-brand-hover hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.pricing.details')}
          <span aria-hidden="true">→</span>
        </a>
      </div>
    </article>
  )
}
