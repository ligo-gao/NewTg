import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { ModelSelector } from './ModelSelector'
import { PriceFields } from './PriceFields'
import {
  convertPriceInput,
  DEFAULT_QUOTA_PER_UNIT,
  normalizePricingMode,
  parsePriceFields,
  priceToInput,
  type PriceFieldKey,
  type PriceFieldValues,
  type PriceUnit,
  type PricingMode,
} from './pricing-utils'

type GroupPricing = components['schemas']['GroupPricing']
type GroupPricingCreateRequest = components['schemas']['GroupPricingCreateRequest']
type GroupPricingUpdateRequest = components['schemas']['GroupPricingUpdateRequest']
type PricingStatus = components['schemas']['PricingStatus']

const EMPTY_PRICES: PriceFieldValues = {
  prompt: '',
  completion: '',
  cacheRead: '',
  cacheWrite: '',
}

export interface GroupPricingMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  override?: GroupPricing
  quotaPerUnit: number
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * F-06-16 分组覆盖价抽屉。创建后分组与模型不可改（API 以字段策略 403
 * 拒绝），且源模型必须已存在于 model_pricing——因此用
 * GET /api/model-pricing/models 驱动的选择器，而不是自由文本。
 *
 * 价格是与模型定价相同四个维度的整数绝对积分；按次计费的源模型隐藏
 * 它从不计费的维度。
 */
export function GroupPricingMutateSheet({
  open,
  mode,
  override,
  quotaPerUnit,
  onOpenChange,
  onSaved,
}: GroupPricingMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const factor = quotaPerUnit > 0 ? quotaPerUnit : DEFAULT_QUOTA_PER_UNIT
  const [group, setGroup] = useState('')
  const [model, setModel] = useState('')
  const [modelMode, setModelMode] = useState<PricingMode>('per_token')
  const [unit, setUnit] = useState<PriceUnit>('credits')
  const [prices, setPrices] = useState<PriceFieldValues>(EMPTY_PRICES)
  const [status, setStatus] = useState<PricingStatus>(1)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  const groupsQuery = useQuery({
    queryKey: ['admin-pricing-groups', 'options'],
    enabled: open,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: { query: { p: 1, page_size: 100 } },
        }),
      ),
    staleTime: 30_000,
  })

  useEffect(() => {
    if (!open) return
    setGroup(override?.group ?? '')
    setModel(override?.model ?? '')
    setModelMode(normalizePricingMode(override?.mode))
    setUnit('credits')
    setPrices({
      prompt: priceToInput(override?.prompt_price, 'credits', factor),
      completion: priceToInput(override?.completion_price, 'credits', factor),
      cacheRead: priceToInput(override?.cache_read_price, 'credits', factor),
      cacheWrite: priceToInput(override?.cache_write_price, 'credits', factor),
    })
    setStatus(override?.status ?? 1)
    setFieldErrors({})
    setFormError(undefined)
    setSubmitting(false)
  }, [open, override, factor])

  const parsed = parsePriceFields(modelMode, prices, unit, factor)

  function changeUnit(next: PriceUnit): void {
    if (next === unit) return
    setPrices((current) => ({
      prompt: convertPriceInput(current.prompt, unit, next, factor),
      completion: convertPriceInput(current.completion, unit, next, factor),
      cacheRead: convertPriceInput(current.cacheRead, unit, next, factor),
      cacheWrite: convertPriceInput(current.cacheWrite, unit, next, factor),
    }))
    setUnit(next)
    setFieldErrors({})
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const errors: Record<string, string> = {}
    if (group.trim() === '') errors.group = t('admin.pricing.override_group_required')
    if (model.trim() === '') errors.model = t('admin.pricing.override_model_required')
    if (modelMode === 'tiered_expr') {
      // 表达式自包含全部价格；服务端拒绝为这类源写入覆盖价（fail-closed），
      // 而不是存一个永远不会生效的价格。历史行可以打开但不能保存。
      errors.model = t('admin.pricing.override_expression_source')
    }
    const priceError = t(
      unit === 'credits'
        ? 'admin.pricing.price_invalid_credits'
        : 'admin.pricing.price_invalid_yuan',
    )
    for (const key of Object.keys(parsed.errors) as PriceFieldKey[]) {
      errors[key] = priceError
    }
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return

    setFormError(undefined)
    setSubmitting(true)
    try {
      // 分组覆盖价是 NOT NULL 的绝对价；空输入表示 0（显式免费），而不是
      // 模型定价的 NULL/兜底三态——覆盖价表无法表达后者。
      //
      // 按次源模型只读 prompt_price：发送保留下来的 completion/cache 值会被
      // 后端当作无效写入拒绝，因此这里把这两个维度钉为 0。
      const prompt = parsed.values.prompt ?? 0
      const completion = modelMode === 'per_request' ? 0 : parsed.values.completion ?? 0
      const cacheRead = modelMode === 'per_request' ? 0 : parsed.values.cacheRead ?? 0
      const cacheWrite = modelMode === 'per_request' ? 0 : parsed.values.cacheWrite ?? 0
      if (mode === 'create') {
        const body: GroupPricingCreateRequest = {
          group: group.trim(),
          model: model.trim(),
          prompt_price: prompt,
          completion_price: completion,
          cache_read_price: cacheRead,
          cache_write_price: cacheWrite,
          status,
        }
        await apiClient.POST('/api/group-pricing', { body })
        push({ tone: 'success', message: t('admin.pricing.override_saved') })
      } else if (override !== undefined) {
        const body: GroupPricingUpdateRequest = {
          updated_at: override.updated_at,
          prompt_price: prompt,
          completion_price: completion,
          cache_read_price: cacheRead,
          cache_write_price: cacheWrite,
          status,
        }
        await apiClient.PATCH('/api/group-pricing/{group}/{model}', {
          params: { path: { group: override.group, model: override.model } },
          body,
        })
        push({ tone: 'success', message: t('admin.pricing.override_saved') })
      }
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
      onSaved()
      onOpenChange(false)
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
        // 创建冲突几乎总是 (group, model) 唯一键；编辑冲突则是
        // updated_at 乐观锁。
        setFormError(
          mode === 'create'
            ? t('admin.pricing.override_duplicate')
            : t('admin.pricing.save_conflict'),
        )
      } else {
        setFormError(t('admin.pricing.save_error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  const groupOptions = (groupsQuery.data?.items ?? []).filter(
    (candidate) => candidate.status === 1 || candidate.name === group,
  )

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={
        mode === 'create'
          ? t('admin.pricing.override_create_title')
          : t('admin.pricing.override_edit_title', {
              group: override?.group ?? '',
              model: override?.model ?? '',
            })
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button type="submit" form="group-pricing-form" disabled={submitting}>
            {submitting ? t('admin.pricing.saving') : t('admin.pricing.save')}
          </Button>
        </>
      }
    >
      <form
        id="group-pricing-form"
        className="flex flex-col gap-4"
        onSubmit={(event) => void save(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <div className="flex flex-col gap-1.5">
          <label htmlFor="override-group" className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.group')}
          </label>
          <select
            id="override-group"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
            value={group}
            disabled={mode === 'edit'}
            aria-invalid={fieldErrors.group !== undefined ? true : undefined}
            onChange={(event) => {
              setGroup(event.target.value)
              setFieldErrors((current) => withoutKey(current, 'group'))
            }}
          >
            <option value="">{t('admin.pricing.group_placeholder')}</option>
            {group !== '' &&
            !groupOptions.some((candidate) => candidate.name === group) ? (
              <option value={group}>{group}</option>
            ) : null}
            {groupOptions.map((candidate) => (
              <option key={candidate.name} value={candidate.name}>
                {candidate.name}
              </option>
            ))}
          </select>
          {fieldErrors.group !== undefined ? (
            <p className="text-xs text-danger">{fieldErrors.group}</p>
          ) : null}
          {groupsQuery.isError ? (
            <div className="flex items-center gap-2 text-xs text-danger">
              <span>{t('admin.pricing.group_load_error')}</span>
              <Button
                variant="ghost"
                className="h-6 px-1 text-xs"
                onClick={() => void groupsQuery.refetch()}
              >
                {t('common.action.retry')}
              </Button>
            </div>
          ) : null}
        </div>

        <ModelSelector
          label={t('admin.pricing.model')}
          value={model}
          disabled={mode === 'edit'}
          includeDisabled
          excludeExpression
          error={fieldErrors.model}
          onChange={(next, option) => {
            setModel(next)
            if (option !== undefined) setModelMode(normalizePricingMode(option.mode))
            setFieldErrors((current) => withoutKey(current, 'model'))
          }}
        />

        <PriceFields
          mode={modelMode}
          unit={unit}
          values={prices}
          errors={{
            prompt: priceErrorText(parsed.errors.prompt, unit, t),
            completion: priceErrorText(parsed.errors.completion, unit, t),
            cacheRead: priceErrorText(parsed.errors.cacheRead, unit, t),
            cacheWrite: priceErrorText(parsed.errors.cacheWrite, unit, t),
          }}
          quotaPerUnit={factor}
          onUnitChange={changeUnit}
          onChange={(key, value) => {
            setPrices((current) => ({ ...current, [key]: value }))
          }}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="override-status" className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.status')}
          </label>
          <select
            id="override-status"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            value={status}
            onChange={(event) => {
              setStatus(event.target.value === '2' ? 2 : 1)
            }}
          >
            <option value="1">{t('admin.pricing.status_enabled')}</option>
            <option value="2">{t('admin.pricing.status_disabled')}</option>
          </select>
        </div>
      </form>
    </Sheet>
  )
}

function withoutKey(
  record: Record<string, string>,
  key: string,
): Record<string, string> {
  const next = { ...record }
  delete next[key]
  return next
}

function priceErrorText(
  invalid: boolean | undefined,
  unit: PriceUnit,
  t: (key: string) => string,
): string | undefined {
  if (invalid !== true) return undefined
  return t(
    unit === 'credits'
      ? 'admin.pricing.price_invalid_credits'
      : 'admin.pricing.price_invalid_yuan',
  )
}
