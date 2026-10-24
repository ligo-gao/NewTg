import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { Dialog } from '@/shared/ui/base/dialog'
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

/** 校验通过后待提交的覆盖价（已把空输入按 0 归一）。 */
interface OverrideValues {
  prompt: number
  completion: number
  cacheRead: number
  cacheWrite: number
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
  const [pendingZero, setPendingZero] = useState<OverrideValues | null>(null)
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
    setPendingZero(null)
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

  /**
   * 校验表单并把空输入按 0 归一。返回 null 表示字段错误已展示。
   *
   * 分组覆盖价是 NOT NULL 的绝对价；空输入表示 0（显式免费），而不是模型
   * 定价的 NULL/兜底三态——覆盖价表无法表达后者。按次源模型只读
   * prompt_price，其余维度钉为 0（后端会拒绝无效写入）。
   */
  function collect(): OverrideValues | null {
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
    if (Object.keys(errors).length > 0) return null

    return {
      prompt: parsed.values.prompt ?? 0,
      completion: modelMode === 'per_request' ? 0 : parsed.values.completion ?? 0,
      cacheRead: modelMode === 'per_request' ? 0 : parsed.values.cacheRead ?? 0,
      cacheWrite: modelMode === 'per_request' ? 0 : parsed.values.cacheWrite ?? 0,
    }
  }

  async function submit(values: OverrideValues): Promise<void> {
    setFormError(undefined)
    setSubmitting(true)
    try {
      if (mode === 'create') {
        const body: GroupPricingCreateRequest = {
          group: group.trim(),
          model: model.trim(),
          prompt_price: values.prompt,
          completion_price: values.completion,
          cache_read_price: values.cacheRead,
          cache_write_price: values.cacheWrite,
          status,
        }
        await apiClient.POST('/api/group-pricing', { body })
        push({ tone: 'success', message: t('admin.pricing.override_saved') })
      } else if (override !== undefined) {
        const body: GroupPricingUpdateRequest = {
          updated_at: override.updated_at,
          prompt_price: values.prompt,
          completion_price: values.completion,
          cache_read_price: values.cacheRead,
          cache_write_price: values.cacheWrite,
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
      } else if (error instanceof ApiError && error.fields !== undefined) {
        // 把 problem.fields 定位回具体输入（F-06-16 的 422 字段错误）。
        const next: Record<string, string> = {}
        for (const field of error.fields) {
          if (field === 'group') {
            next.group = t('admin.pricing.override_group_invalid')
          } else if (field === 'model') {
            next.model = t('admin.pricing.override_model_invalid')
          } else if (field === 'prompt_price') {
            next.prompt = t('admin.pricing.price_server_rejected')
          } else if (field === 'completion_price') {
            next.completion = t('admin.pricing.price_server_rejected')
          } else if (field === 'cache_read_price') {
            next.cacheRead = t('admin.pricing.price_server_rejected')
          } else if (field === 'cache_write_price') {
            next.cacheWrite = t('admin.pricing.price_server_rejected')
          }
        }
        setFieldErrors(next)
        if (Object.keys(next).length === 0) setFormError(t('admin.pricing.save_error'))
      } else {
        setFormError(t('admin.pricing.save_error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  async function save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const values = collect()
    if (values === null) return
    // 全 0 覆盖价是合法但危险的意图（该分组下此模型免费）；确认后再提交，
    // 避免「留空」被误当成默认价（本表单的空 = 0，不是兜底）。
    if (
      values.prompt === 0 &&
      values.completion === 0 &&
      values.cacheRead === 0 &&
      values.cacheWrite === 0
    ) {
      setPendingZero(values)
      return
    }
    await submit(values)
  }

  const groupOptions = (groupsQuery.data?.items ?? []).filter(
    (candidate) => candidate.status === 1 || candidate.name === group,
  )

  /**
   * 确认对话框是栈顶模态。Dialog 与 Sheet 都在 document 上监听 Escape，
   * 若不在 Sheet 侧拦截，一次 Escape 会同时关掉两个模态并丢掉已填输入
   * （N1）。确认框打开期间只忽略「关闭」请求，正常打开不受影响。
   */
  function handleSheetOpenChange(nextOpen: boolean): void {
    if (!nextOpen && pendingZero !== null) return
    onOpenChange(nextOpen)
  }

  return (
    <>
      <Sheet
        open={open}
        onOpenChange={handleSheetOpenChange}
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
            <Button variant="secondary" onClick={() => handleSheetOpenChange(false)}>
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
          <label htmlFor="override-group" className="text-meta font-medium text-ink-1">
            {t('admin.pricing.group')}
          </label>
          <select
            id="override-group"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
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
            <p className="text-meta text-danger">{fieldErrors.group}</p>
          ) : null}
          {groupsQuery.isError ? (
            <div className="flex items-center gap-2 text-meta text-danger">
              <span>{t('admin.pricing.group_load_error')}</span>
              <Button
                variant="ghost"
                className="h-6 px-1 text-meta"
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
            prompt:
              priceErrorText(parsed.errors.prompt, unit, t) ?? fieldErrors.prompt,
            completion:
              priceErrorText(parsed.errors.completion, unit, t) ??
              fieldErrors.completion,
            cacheRead:
              priceErrorText(parsed.errors.cacheRead, unit, t) ??
              fieldErrors.cacheRead,
            cacheWrite:
              priceErrorText(parsed.errors.cacheWrite, unit, t) ??
              fieldErrors.cacheWrite,
          }}
          quotaPerUnit={factor}
          context="group"
          onUnitChange={changeUnit}
          onChange={(key, value) => {
            setPrices((current) => ({ ...current, [key]: value }))
            setFieldErrors((current) => withoutKey(current, key))
          }}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="override-status" className="text-meta font-medium text-ink-1">
            {t('admin.pricing.status')}
          </label>
          <select
            id="override-status"
            className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
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

      <Dialog
        open={pendingZero !== null}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setPendingZero(null)
        }}
        title={t('admin.pricing.override_all_zero_title')}
        description={t('admin.pricing.override_all_zero_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setPendingZero(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              disabled={submitting}
              onClick={() => {
                const values = pendingZero
                setPendingZero(null)
                if (values !== null) void submit(values)
              }}
            >
              {t('admin.pricing.override_all_zero_confirm')}
            </Button>
          </>
        }
      />
    </>
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
