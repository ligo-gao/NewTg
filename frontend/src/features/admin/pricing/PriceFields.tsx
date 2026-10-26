import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import {
  creditsToYuanInput,
  isPerRequest,
  parsePriceInput,
  type PriceFieldKey,
  type PriceFieldValues,
  type PriceUnit,
  type PricingMode,
} from './pricing-utils'

export interface PriceFieldsProps {
  mode: PricingMode
  unit: PriceUnit
  values: PriceFieldValues
  errors: Partial<Record<PriceFieldKey, string>>
  quotaPerUnit: number
  /**
   * `group` 的空输入语义是 0（显式免费），`model` 的空输入是 NULL（走
   * 默认价兜底）；提示文案必须随上下文变化，否则「留空 = 默认」会误导
   * 覆盖价操作者。
   */
  context?: 'model' | 'group'
  onUnitChange: (unit: PriceUnit) => void
  onChange: (key: PriceFieldKey, value: string) => void
}

interface FieldSpec {
  key: PriceFieldKey
  labelKey: string
}

/**
 * 模型抽屉与分组覆盖价抽屉共用的价格编辑器。
 *
 * 单位：API 永远存整数积分；操作者可以按积分或元输入。元编辑器通过
 * quota_per_unit（默认 100）换算，并显示即将提交的精确积分，避免操作者
 * 去信任一个半可见的换算。空输入刻意保持为空（= NULL / 未设置，R9）：
 * 什么都不填不能静默变成显式免费价。
 */
export function PriceFields({
  mode,
  unit,
  values,
  errors,
  quotaPerUnit,
  context = 'model',
  onUnitChange,
  onChange,
}: PriceFieldsProps) {
  const { t, locale } = useI18n()
  if (mode === 'tiered_expr') return null

  const perRequest = isPerRequest(mode)
  const fields: FieldSpec[] = perRequest
    ? [{ key: 'prompt', labelKey: 'admin.pricing.price_per_request' }]
    : [
        // 两档输入并排：左 = 缓存未命中（全价），右 = 缓存命中（折扣价）。
        { key: 'prompt', labelKey: 'admin.pricing.price_prompt' },
        { key: 'cacheRead', labelKey: 'admin.pricing.price_cache_read' },
        { key: 'completion', labelKey: 'admin.pricing.price_completion' },
        { key: 'cacheWrite', labelKey: 'admin.pricing.price_cache_write' },
      ]

  const promptCredits = parsePriceInput(values.prompt, unit, quotaPerUnit).value
  const cacheReadCredits = parsePriceInput(values.cacheRead, unit, quotaPerUnit).value
  // 只有两档都填了才有比例可谈；未命中价为 0 时比例没有意义（分母为零）。
  const hitRatio =
    promptCredits !== null && promptCredits > 0 && cacheReadCredits !== null
      ? cacheReadCredits / promptCredits
      : null
  const hitRatioText =
    hitRatio !== null
      ? t('admin.pricing.price_hit_ratio', {
          percent: `${(hitRatio * 100).toFixed(2)}%`,
        })
      : values.cacheRead.trim() === ''
        ? context === 'group'
          ? t('admin.pricing.price_hit_ratio_group_empty')
          : t('admin.pricing.price_hit_ratio_empty')
        : null

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h3 className="text-body font-semibold text-ink-1">{t('admin.pricing.price_section')}</h3>
        <div role="group" aria-label={t('admin.pricing.price_unit')} className="flex gap-1">
          <Button
            variant={unit === 'credits' ? 'secondary' : 'ghost'}
            aria-pressed={unit === 'credits'}
            className="h-7 px-2 text-meta"
            onClick={() => {
              onUnitChange('credits')
            }}
          >
            {t('admin.pricing.unit_credits')}
          </Button>
          <Button
            variant={unit === 'yuan' ? 'secondary' : 'ghost'}
            aria-pressed={unit === 'yuan'}
            className="h-7 px-2 text-meta"
            onClick={() => {
              onUnitChange('yuan')
            }}
          >
            {t('admin.pricing.unit_yuan')}
          </Button>
        </div>
      </div>

      <p className="text-meta text-ink-3">
        {context === 'group'
          ? perRequest
            ? t('admin.pricing.price_hint_group_request')
            : t('admin.pricing.price_hint_group_tokens')
          : perRequest
            ? t('admin.pricing.price_hint_request')
            : t('admin.pricing.price_hint_tokens')}
      </p>

      <div className={perRequest ? 'grid grid-cols-1 gap-3' : 'grid grid-cols-2 gap-3'}>
        {fields.map((field) => {
          const raw = values[field.key]
          const parsedCredits =
            unit === 'yuan' && raw.trim() !== ''
              ? Number(raw) * quotaPerUnit
              : undefined
          const conversion =
            parsedCredits !== undefined && Number.isFinite(parsedCredits)
              ? t('admin.pricing.price_conversion', {
                  credits: Math.round(parsedCredits).toLocaleString(locale),
                })
              : undefined
          return (
            <div key={field.key} className="flex flex-col gap-1">
              <TextField
                id={`pricing-${field.key}-price`}
                label={t(field.labelKey)}
                inputMode={unit === 'credits' ? 'numeric' : 'decimal'}
                value={raw}
                placeholder={
                  context === 'group'
                    ? t('admin.pricing.price_placeholder_group')
                    : t('admin.pricing.price_placeholder')
                }
                onChange={(event) => {
                  onChange(field.key, event.target.value)
                }}
                error={errors[field.key]}
              />
              {unit === 'yuan' && raw.trim() !== '' ? (
                <p className="text-meta text-ink-3">{conversion}</p>
              ) : null}
              {field.key === 'cacheRead' && hitRatioText !== null ? (
                <p className="text-meta text-ink-3" data-testid="pricing-hit-ratio">
                  {hitRatioText}
                </p>
              ) : null}
            </div>
          )
        })}
      </div>

      {hitRatio !== null && hitRatio > 1 ? (
        <p
          role="status"
          className="rounded-md border border-warning-line bg-warning-bg px-3 py-2 text-meta text-ink-1"
        >
          {t('admin.pricing.price_cache_hit_above_miss')}
        </p>
      ) : null}

      {unit === 'yuan' ? (
        <p className="text-meta text-ink-3">
          {t('admin.pricing.unit_yuan_hint', { rate: creditsToYuanInput(1, quotaPerUnit) })}
        </p>
      ) : null}
    </section>
  )
}
