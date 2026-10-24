/**
 * 「价签」——本页最重要的版式单元（modules-09 UI 子模块 B §5）。
 *
 * 每一行由「标签 · 数值 · 单位」三段构成，数值列定宽右对齐：基准价与
 * 分组实付价共用同一数值列，用户才能竖着读出差价。
 *
 * 冻结口径：
 * - 金额来自服务端的 int64 字符串，全程 BigInt（format.ts）；
 * - 前端**不做**分组倍率乘法，`group_price` 原样展示；
 * - `null` → `—`，显式 `0` → `0`，全 0 才由服务端标记 `is_free`。
 */

import type { PublicModelCard, PublicPricingMeta } from './api'
import {
  convertCreditsPerK,
  currencyReference,
  formatCredit,
  formatRatio,
  rechargeReference,
} from './format'
import type { PricingTokenUnit } from './params'
import { Tag } from '@/shared/ui/tag'
import { useI18n } from '@/shared/lib/i18n-context'

export interface PriceRegisterProps {
  card: PublicModelCard
  meta?: PublicPricingMeta
  tokenUnit: PricingTokenUnit
  dualCurrency: boolean
  rechargePrice: boolean
  /** 当前分组价是否就是登录用户自己的分组（决定徽章文案）。 */
  isOwnGroup?: boolean
}

function unitKey(mode: string, tokenUnit: PricingTokenUnit): string {
  if (mode === 'per_request') return 'public.pricing.unit_credits_call'
  return tokenUnit === 'K'
    ? 'public.pricing.unit_credits_k'
    : 'public.pricing.unit_credits_m'
}

function PriceLine({
  label,
  value,
  unit,
  converted = false,
  zero = false,
}: {
  label: string
  value: string
  unit: string
  converted?: boolean
  zero?: boolean
}) {
  const { t } = useI18n()
  return (
    <div className="grid grid-cols-[64px_88px_1fr] items-center gap-2 text-meta leading-6">
      <span className="text-badge text-ink-2">{label}</span>
      <span
        className={[
          'text-right font-mono text-card-title font-semibold tabular-nums',
          zero ? 'text-amount-zero' : 'text-ink-1',
        ].join(' ')}
      >
        {value}
      </span>
      <span className="flex items-center gap-1.5 text-meta text-ink-2">
        {unit}
        {converted ? (
          <Tag variant="neutral">{t('public.pricing.unit_converted')}</Tag>
        ) : null}
      </span>
    </div>
  )
}

export function PriceRegister({
  card,
  meta,
  tokenUnit,
  dualCurrency,
  rechargePrice,
  isOwnGroup = false,
}: PriceRegisterProps) {
  const { t, locale } = useI18n()
  const unit = t(unitKey(card.mode, tokenUnit))

  if (card.is_free) {
    return (
      <div className="flex min-h-[128px] flex-col justify-center gap-2 border-y border-border-hairline py-3">
        <Tag variant="success">{t('public.pricing.free')}</Tag>
        <p className="text-meta leading-5 text-ink-2">
          {t('public.pricing.free_note')}
        </p>
      </div>
    )
  }

  /** 统一取值：K 视图做换算并打「换算」角标，M 视图保持整数。 */
  const display = (raw: string | null): { text: string; converted: boolean } => {
    if (tokenUnit === 'K') {
      const converted = convertCreditsPerK(raw, locale)
      return { text: converted ?? '—', converted: converted !== null }
    }
    return { text: formatCredit(raw, locale), converted: false }
  }

  const prompt = display(card.prompt_price)
  const completion = display(card.completion_price)
  const cacheRead = display(card.cache_read_price)
  const cacheWrite = display(card.cache_write_price)

  const reference = dualCurrency
    ? currencyReference(card.prompt_price, meta, locale)
    : null
  const recharge = rechargePrice
    ? rechargeReference(card.prompt_price, meta, locale)
    : null

  return (
    <div className="flex min-h-[128px] flex-col gap-0 border-y border-border-hairline py-3">
      {card.mode === 'per_request' ? (
        <PriceLine
          label={t('public.pricing.price_per_call')}
          value={prompt.text}
          unit={unit}
          converted={prompt.converted}
          zero={card.prompt_price === '0'}
        />
      ) : (
        <>
          <PriceLine
            label={t('public.pricing.price_input')}
            value={prompt.text}
            unit={unit}
            converted={prompt.converted}
            zero={card.prompt_price === '0'}
          />
          <PriceLine
            label={t('public.pricing.price_output')}
            value={completion.text}
            unit={unit}
            converted={completion.converted}
            zero={card.completion_price === '0'}
          />
          <PriceLine
            label={t('public.pricing.price_cache_read')}
            value={cacheRead.text}
            unit={unit}
            converted={cacheRead.converted}
            zero={card.cache_read_price === '0'}
          />
          <PriceLine
            label={t('public.pricing.price_cache_write')}
            value={cacheWrite.text}
            unit={unit}
            converted={cacheWrite.converted}
            zero={card.cache_write_price === '0'}
          />
        </>
      )}

      {card.group_price !== null ? (
        <div className="mt-1 grid grid-cols-[64px_88px_1fr] items-center gap-2 border-t border-border-hairline pt-1 text-meta leading-6">
          <span className="flex min-w-0 items-center gap-1 border-l-2 border-brand pl-1.5 text-badge text-brand">
            <span className="truncate">
              {isOwnGroup
                ? t('public.pricing.your_group_label', {
                    group: card.group_price.group,
                  })
                : t('public.pricing.preview_group_label', {
                    group: card.group_price.group,
                  })}
            </span>
          </span>
          <span className="text-right font-mono text-card-title font-semibold tabular-nums text-ink-1">
            {display(card.group_price.prompt).text}
          </span>
          <span className="flex items-center gap-1.5 text-meta text-ink-2">
            {unit}
            {formatRatio(card.group_price.ratio) !== null ? (
              <Tag variant="neutral">{formatRatio(card.group_price.ratio)}</Tag>
            ) : null}
            {card.group_price.source === 'override' ? (
              <span title={t('public.pricing.override')} className="text-meta text-ink-3">
                {t('public.pricing.override')}
              </span>
            ) : null}
          </span>
        </div>
      ) : null}

      {reference !== null || recharge !== null ? (
        <div className="mt-auto flex flex-col gap-0.5 pt-1.5 text-meta leading-5 text-ink-2">
          {reference !== null ? (
            <p>
              {reference.usd === null
                ? t('public.pricing.reference_cny', {
                    cny: reference.cny,
                    unit,
                  })
                : t('public.pricing.reference_both', {
                    cny: reference.cny,
                    usd: reference.usd,
                    unit,
                  })}
              <span className="ml-1 text-ink-3">
                {t('public.pricing.reference_only')}
              </span>
            </p>
          ) : null}
          {recharge !== null ? (
            <p>
              {t('public.pricing.recharge_ref', { cny: recharge, unit })}
            </p>
          ) : null}
        </div>
      ) : null}

      {card.group_price !== null && !isOwnGroup ? (
        <p className="pt-1 text-meta leading-5 text-ink-3">
          {t('public.pricing.preview_note')}
        </p>
      ) : null}
    </div>
  )
}
