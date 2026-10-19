import { useEffect, useState, type FormEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { PriceFields } from './PriceFields'
import { PricePreview } from './PricePreview'
import {
  convertPriceInput,
  DEFAULT_QUOTA_PER_UNIT,
  normalizePricingMode,
  parsePriceFields,
  priceToInput,
  pricingModeKey,
  type PriceFieldKey,
  type PriceFieldValues,
  type PriceUnit,
  type PricingMode,
} from './pricing-utils'

type ModelPricing = components['schemas']['ModelPricing']
type ModelPricingCreateRequest = components['schemas']['ModelPricingCreateRequest']
type ModelPricingUpdateRequest = components['schemas']['ModelPricingUpdateRequest']
type PricingStatus = components['schemas']['PricingStatus']

const MODEL_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/
const MODES: readonly PricingMode[] = ['per_token', 'per_request', 'tiered_expr']

const EMPTY_PRICES: PriceFieldValues = {
  prompt: '',
  completion: '',
  cacheRead: '',
  cacheWrite: '',
}

export interface ModelPricingMutateSheetProps {
  open: boolean
  mode: 'create' | 'edit'
  pricing?: ModelPricing
  quotaPerUnit: number
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * F-06-14 创建/编辑抽屉。切换模式时，隐藏的价格字段保留在组件状态里
 * （UI §8.5），但不会进入请求，因为后端拒绝带 completion/cache 价格的
 * per_request 行。
 *
 * 乐观锁：每次编辑都回传行上的 updated_at；409 时保留操作者的输入、显示
 * 冲突文案并刷新列表，重新打开抽屉即可从新版本继续（web-00 §5.6——
 * 不自动重试、不回滚已输入内容）。
 */
export function ModelPricingMutateSheet({
  open,
  mode,
  pricing,
  quotaPerUnit,
  onOpenChange,
  onSaved,
}: ModelPricingMutateSheetProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const factor = quotaPerUnit > 0 ? quotaPerUnit : DEFAULT_QUOTA_PER_UNIT
  const [model, setModel] = useState('')
  const [pricingMode, setPricingMode] = useState<PricingMode>('per_token')
  const [expr, setExpr] = useState('')
  const [unit, setUnit] = useState<PriceUnit>('credits')
  const [prices, setPrices] = useState<PriceFieldValues>(EMPTY_PRICES)
  const [status, setStatus] = useState<PricingStatus>(1)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [previewInvalid, setPreviewInvalid] = useState(false)
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setModel(pricing?.model ?? '')
    setPricingMode(normalizePricingMode(pricing?.mode))
    setExpr(pricing?.billing_expr ?? '')
    setUnit('credits')
    setPrices({
      prompt: priceToInput(pricing?.prompt_price, 'credits', factor),
      completion: priceToInput(pricing?.completion_price, 'credits', factor),
      cacheRead: priceToInput(pricing?.cache_read_price, 'credits', factor),
      cacheWrite: priceToInput(pricing?.cache_write_price, 'credits', factor),
    })
    setStatus(pricing?.status ?? 1)
    setFieldErrors({})
    setPreviewInvalid(false)
    setFormError(undefined)
    setSubmitting(false)
  }, [open, pricing, factor])

  const parsed = parsePriceFields(pricingMode, prices, unit, factor)
  const expressionError =
    fieldErrors.expr ??
    (previewInvalid && pricingMode === 'tiered_expr'
      ? t('admin.pricing.expression_invalid')
      : undefined)

  function changeUnit(next: PriceUnit): void {
    if (next === unit) return
    // 用当前系数换算可见文本，保证切换单位不改变价格。非法文本原样保留
    // 让操作者修正；把校验错误藏在开关后面只会更糟。
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
    if (mode === 'create' && !MODEL_PATTERN.test(model.trim())) {
      errors.model = t('admin.pricing.model_invalid')
    }
    if (pricingMode === 'tiered_expr' && expr.trim() === '') {
      errors.expr = t('admin.pricing.expression_required')
    }
    // 预览端点与保存使用同一解析器：预览已判定非法时不再发保存请求，
    // 避免一次注定 400 的写入。
    if (pricingMode === 'tiered_expr' && previewInvalid) {
      errors.expr = t('admin.pricing.expression_invalid')
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
      if (mode === 'create') {
        const body: ModelPricingCreateRequest = {
          model: model.trim(),
          mode: pricingMode,
          status,
        }
        if (pricingMode === 'tiered_expr') {
          body.billing_expr = expr.trim()
        } else {
          body.prompt_price = parsed.values.prompt
          if (pricingMode === 'per_token') {
            body.completion_price = parsed.values.completion
            body.cache_read_price = parsed.values.cacheRead
            body.cache_write_price = parsed.values.cacheWrite
          }
        }
        await apiClient.POST('/api/model-pricing', { body })
        push({ tone: 'success', message: t('admin.pricing.saved') })
      } else if (pricing !== undefined) {
        const body: ModelPricingUpdateRequest = {
          updated_at: pricing.updated_at,
          mode: pricingMode,
        }
        if (pricingMode === 'tiered_expr') {
          body.billing_expr = expr.trim()
        } else {
          body.prompt_price = parsed.values.prompt
          if (pricingMode === 'per_token') {
            body.completion_price = parsed.values.completion
            body.cache_read_price = parsed.values.cacheRead
            body.cache_write_price = parsed.values.cacheWrite
          }
          // 退出表达式模式只有在显式清空列（billing_expr: null）时才无歧义；
          // 只给 mode 的退出会被字段策略以 422 拒绝（T7 / handlers_pricing.go）。
          if (pricing.billing_expr !== null && pricing.billing_expr !== undefined) {
            body.billing_expr = null
          }
        }
        if (status !== pricing.status) body.status = status
        await apiClient.PATCH('/api/model-pricing/{model}', {
          params: { path: { model: pricing.model } },
          body,
        })
        push({ tone: 'success', message: t('admin.pricing.saved') })
      }
      await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      // 模型模式/价格变化会影响分组覆盖价的展示（mode/source_status），
      // 删除则会级联删除覆盖价；模型选择器也缓存了 mode/status。
      await queryClient.invalidateQueries({ queryKey: ['admin-pricing-models'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
      onSaved()
      onOpenChange(false)
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 创建 409 几乎总是 model 唯一键；更新 409 是 updated_at 乐观锁。
        if (mode === 'create' || error.fields?.includes('model') === true) {
          setFieldErrors((current) => ({
            ...current,
            model: t('admin.pricing.model_exists'),
          }))
        } else {
          // 刷新列表，让下一次编辑从胜出版本开始；抽屉保持打开并保留
          // 操作者的输入（web-00 §5.6）。
          await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
          setFormError(t('admin.pricing.save_conflict'))
        }
      } else if (error instanceof ApiError && error.fields !== undefined) {
        // 把 problem.fields 定位回编辑器/价格输入（F-06-14 字段级 400/422）。
        const next: Record<string, string> = {}
        for (const field of error.fields) {
          if (field === 'billing_expr') {
            next.expr = t('admin.pricing.expression_invalid')
          } else if (field === 'model') {
            next.model =
              mode === 'create'
                ? t('admin.pricing.model_exists')
                : t('admin.pricing.model_invalid')
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

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={
        mode === 'create'
          ? t('admin.pricing.create_title')
          : t('admin.pricing.edit_title', { model: pricing?.model ?? '' })
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button type="submit" form="model-pricing-form" disabled={submitting}>
            {submitting
              ? mode === 'create'
                ? t('admin.pricing.creating')
                : t('admin.pricing.saving')
              : t('admin.pricing.save')}
          </Button>
        </>
      }
    >
      <form
        id="model-pricing-form"
        className="flex flex-col gap-4"
        onSubmit={(event) => void save(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <TextField
          label={t('admin.pricing.model')}
          value={model}
          disabled={mode === 'edit'}
          maxLength={128}
          placeholder={t('admin.pricing.model_placeholder')}
          hint={mode === 'create' ? t('admin.pricing.model_hint') : undefined}
          error={fieldErrors.model}
          onChange={(event) => {
            setModel(event.target.value)
            setFieldErrors((current) => withoutKey(current, 'model'))
          }}
        />

        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.mode')}
          </span>
          <div
            role="group"
            aria-label={t('admin.pricing.mode')}
            className="flex flex-wrap gap-1"
          >
            {MODES.map((candidate) => (
              <Button
                key={candidate}
                variant={pricingMode === candidate ? 'secondary' : 'ghost'}
                aria-pressed={pricingMode === candidate}
                className="h-8 px-2 text-[13px]"
                onClick={() => {
                  setPricingMode(candidate)
                  setFieldErrors((current) => withoutKey(current, 'expr'))
                }}
              >
                {t(pricingModeKey(candidate))}
              </Button>
            ))}
          </div>
        </div>

        {pricingMode === 'tiered_expr' ? (
          <div className="flex flex-col gap-1.5">
            <label
              htmlFor="pricing-expression"
              className="text-[13px] font-medium text-ink-1"
            >
              {t('admin.pricing.expression')}
            </label>
            <textarea
              id="pricing-expression"
              rows={4}
              spellCheck={false}
              value={expr}
              aria-invalid={expressionError !== undefined ? true : undefined}
              aria-describedby={
                expressionError !== undefined ? 'pricing-expression-error' : undefined
              }
              className={[
                'w-full rounded-lg border bg-surface-card px-3 py-2 font-mono text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                expressionError !== undefined
                  ? 'border-danger'
                  : 'border-border-default',
              ].join(' ')}
              placeholder={t('admin.pricing.expression_placeholder')}
              onChange={(event) => {
                setExpr(event.target.value)
                setPreviewInvalid(false)
                setFieldErrors((current) => withoutKey(current, 'expr'))
              }}
            />
            {expressionError !== undefined ? (
              <p id="pricing-expression-error" className="text-xs text-danger">
                {expressionError}
              </p>
            ) : null}
            <p className="text-xs text-ink-3">
              {t('admin.pricing.expression_hint')}
            </p>
            <p className="font-mono text-xs text-ink-3">
              {t('admin.pricing.expression_variables')}
            </p>
          </div>
        ) : (
          <PriceFields
            mode={pricingMode}
            unit={unit}
            values={prices}
            errors={{
              prompt:
                priceErrorText(parsed.errors.prompt, unit, t) ??
                fieldErrors.prompt,
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
            onUnitChange={changeUnit}
            onChange={(key, value) => {
              setPrices((current) => ({ ...current, [key]: value }))
              setFieldErrors((current) => withoutKey(current, key))
            }}
          />
        )}

        <PricePreview
          mode={pricingMode}
          expr={expr}
          promptPrice={parsed.values.prompt}
          completionPrice={parsed.values.completion}
          cacheReadPrice={parsed.values.cacheRead}
          cacheWritePrice={parsed.values.cacheWrite}
          model={model}
          onValidationChange={setPreviewInvalid}
        />

        <div className="flex flex-col gap-1.5">
          <label htmlFor="pricing-status" className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.status')}
          </label>
          <select
            id="pricing-status"
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
