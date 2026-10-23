import { useEffect, useRef, useState, type FormEvent } from 'react'
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

import { ModelCombobox } from './ModelCombobox'
import { PriceFields } from './PriceFields'
import { PricePreview } from './PricePreview'
import { PricingBand } from './PricingBand'
import { StrategyPreview } from './StrategyPreview'
import { TimeWindowEditor } from './TimeWindowEditor'
import {
  convertPriceInput,
  createDefaultTimeConfig,
  DEFAULT_QUOTA_PER_UNIT,
  DEFAULT_STRATEGY_TIMEZONE,
  formatMultiplierPercent,
  isValidModelName,
  matchLocalTimeRule,
  normalizePricingMode,
  parsePriceFields,
  priceToInput,
  pricingModeKey,
  strategyFromTimeConfig,
  timeConfigFromStrategy,
  wallClockParts,
  wallClockToRfc3339,
  zonedNow,
  type PriceFieldKey,
  type PriceFieldValues,
  type PriceUnit,
  type PricingMode,
  type TimeConfigDraft,
} from './pricing-utils'

type ModelPricing = components['schemas']['ModelPricing']
type ModelPricingCreateRequest = components['schemas']['ModelPricingCreateRequest']
type ModelPricingUpdateRequest = components['schemas']['ModelPricingUpdateRequest']
type PricingStatus = components['schemas']['PricingStatus']
type PricingCandidate = components['schemas']['PricingCandidate']

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
 * F-06-14 定价工作台（抽屉形态）。
 *
 * 左侧是模型选择、计费方式、基础费率、时段价格与 7×24 时段带；右侧是
 * 粘性试算面板。列表响应只带 has_strategy/strategy_kind，编辑已配置时段
 * 的行时按需读取 GET /api/model-pricing/{model} 回填完整窗口。
 *
 * 乐观锁：每次编辑都回传行上的 updated_at；409 时保留操作者的输入、显示
 * 冲突文案并刷新列表（web-00 §5.6——不自动重试、不回滚已输入内容）。
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
  const [timeConfig, setTimeConfig] = useState<TimeConfigDraft>(createDefaultTimeConfig)
  const [previewAt, setPreviewAt] = useState('')
  /** 创建模式下选中「已定价」候选时加载的完整行；保存走 PATCH。 */
  const [existing, setExisting] = useState<ModelPricing | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [previewInvalid, setPreviewInvalid] = useState(false)
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)
  const strategyTouched = useRef(false)
  const hydratedKey = useRef<string | null>(null)

  const detailQuery = useQuery({
    queryKey: ['admin-model-pricing-detail', pricing?.model ?? ''],
    // 列表只返回策略摘要；只有确实存在策略时才需要完整窗口。
    enabled: open && mode === 'edit' && pricing?.has_strategy === true,
    retry: false,
    staleTime: 0,
    queryFn: async () => {
      if (pricing === undefined) throw new Error('missing pricing row')
      return requireData(
        await apiClient.GET('/api/model-pricing/{model}', {
          params: { path: { model: pricing.model } },
        }),
      )
    },
  })

  useEffect(() => {
    if (!open) return
    strategyTouched.current = false
    hydratedKey.current = null
    setExisting(null)
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
    setTimeConfig(createDefaultTimeConfig())
    setPreviewAt(zonedNow(DEFAULT_STRATEGY_TIMEZONE).value)
    setFieldErrors({})
    setPreviewInvalid(false)
    setFormError(undefined)
    setSubmitting(false)
  }, [open, pricing, factor])

  // 完整策略到达后只回填时段草稿：价格/状态已由列表行给出，不能覆盖
  // 操作者在这段时间里已经输入的改动。
  useEffect(() => {
    const row = detailQuery.data
    if (!open || row === undefined) return
    const key = `${row.model}@${row.updated_at}`
    if (hydratedKey.current === key) return
    hydratedKey.current = key
    if (strategyTouched.current) return
    setTimeConfig(timeConfigFromStrategy(row.strategy))
  }, [detailQuery.data, open])

  const editing = mode === 'edit' ? pricing : (existing ?? undefined)
  const detailLoading =
    mode === 'edit' && pricing?.has_strategy === true && detailQuery.isPending
  /** 读取完整策略失败时必须阻止保存，否则会把已有策略当作「未配置」清空。 */
  const detailFailed =
    mode === 'edit' && pricing?.has_strategy === true && detailQuery.isError

  const parsed = parsePriceFields(pricingMode, prices, unit, factor)
  const expressionError =
    fieldErrors.expr ??
    (previewInvalid && pricingMode === 'tiered_expr'
      ? t('admin.pricing.expression_invalid')
      : undefined)

  const strategyResult =
    pricingMode === 'tiered_expr'
      ? { strategy: null, errors: {} as Record<string, boolean> }
      : strategyFromTimeConfig(timeConfig)

  const strategyErrors = translateStrategyErrors(strategyResult.errors, t)

  const previewWallClock = wallClockParts(previewAt)
  const localMatch =
    timeConfig.enabled && previewWallClock !== null
      ? matchLocalTimeRule(
          timeConfig,
          previewWallClock.day,
          previewWallClock.minutes,
        )
      : { window: '', multiplier: 1 }

  function fillFromRow(row: ModelPricing): void {
    setModel(row.model)
    setPricingMode(normalizePricingMode(row.mode))
    setExpr(row.billing_expr ?? '')
    setUnit('credits')
    setPrices({
      prompt: priceToInput(row.prompt_price, 'credits', factor),
      completion: priceToInput(row.completion_price, 'credits', factor),
      cacheRead: priceToInput(row.cache_read_price, 'credits', factor),
      cacheWrite: priceToInput(row.cache_write_price, 'credits', factor),
    })
    setStatus(row.status)
    setTimeConfig(timeConfigFromStrategy(row.strategy))
  }

  async function selectCandidate(candidate: PricingCandidate): Promise<void> {
    setFormError(undefined)
    if (!candidate.priced) {
      setExisting(null)
      setTimeConfig(createDefaultTimeConfig())
      setPreviewAt(zonedNow(DEFAULT_STRATEGY_TIMEZONE).value)
      return
    }
    try {
      const row = await queryClient.fetchQuery({
        queryKey: ['admin-model-pricing-detail', candidate.model],
        queryFn: async () =>
          requireData(
            await apiClient.GET('/api/model-pricing/{model}', {
              params: { path: { model: candidate.model } },
            }),
          ),
        staleTime: 0,
      })
      hydratedKey.current = `${row.model}@${row.updated_at}`
      setExisting(row)
      fillFromRow(row)
    } catch {
      setFormError(t('admin.pricing.model_load_error'))
    }
  }

  function changeTimeConfig(next: TimeConfigDraft): void {
    strategyTouched.current = true
    setTimeConfig(next)
    setFieldErrors((current) => withoutKeys(current, (key) => isStrategyErrorKey(key)))
  }

  /**
   * 保存前 preview-strategy 的字段级校验：把 `strategy.time.windows[0].start`
   * 定位回时段编辑器；无法定位的保留表单级提示。
   */
  function applyStrategyFieldErrors(fields: string[] | undefined): void {
    const next: Record<string, string> = {}
    let unmapped = false
    for (const field of fields ?? []) {
      if (!field.startsWith('strategy')) {
        unmapped = true
        continue
      }
      const mapped = mapStrategyField(field)
      if (mapped === null) {
        unmapped = true
      } else {
        next[mapped.key] = t(mapped.messageKey)
      }
    }
    if (Object.keys(next).length > 0) {
      setFieldErrors((current) => ({ ...current, ...next }))
    }
    if (unmapped || Object.keys(next).length === 0) {
      setFormError(t('admin.pricing.strategy_server_rejected'))
    }
  }

  function changeUnit(next: PriceUnit): void {    if (next === unit) return
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
    if (!editing && !isValidModelName(model)) {
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
    Object.assign(errors, strategyErrors)
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return
    if (detailLoading || detailFailed) return

    setFormError(undefined)
    setSubmitting(true)
    try {
      const parsedStrategy = strategyResult.strategy
      // 服务端一致性校验：本地试算不含节假日日历，保存前用与结算同一套
      // 解析器复核一次；失败则不写入，避免「界面显示低谷、结算按高峰」。
      if (parsedStrategy !== null) {
        const at = wallClockToRfc3339(previewAt, timeConfig.timezone)
        try {
          await apiClient.POST('/api/model-pricing:preview-strategy', {
            body: {
              ...(model.trim() === '' ? {} : { model: model.trim() }),
              mode: pricingMode === 'per_request' ? 'per_request' : 'per_token',
              prices: {
                ...(parsed.values.prompt !== null
                  ? { prompt_price: parsed.values.prompt }
                  : {}),
                ...(pricingMode === 'per_token' && parsed.values.completion !== null
                  ? { completion_price: parsed.values.completion }
                  : {}),
                ...(pricingMode === 'per_token' && parsed.values.cacheRead !== null
                  ? { cache_read_price: parsed.values.cacheRead }
                  : {}),
                ...(pricingMode === 'per_token' && parsed.values.cacheWrite !== null
                  ? { cache_write_price: parsed.values.cacheWrite }
                  : {}),
              },
              strategy: parsedStrategy,
              ...(at === null ? {} : { at }),
              input_tokens: 128_000,
              output_tokens: 4_000,
              cache_read_tokens: 64_000,
            },
          })
        } catch (error) {
          if (error instanceof ApiError && error.kind === 'validation') {
            applyStrategyFieldErrors(error.fields)
          } else {
            setFormError(t('admin.pricing.strategy_check_failed'))
          }
          return
        }
      }

      if (editing === undefined) {
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
        if (parsedStrategy !== null) body.strategy = parsedStrategy
        await apiClient.POST('/api/model-pricing', { body })
        push({ tone: 'success', message: t('admin.pricing.saved') })
      } else {
        const body: ModelPricingUpdateRequest = {
          updated_at: editing.updated_at,
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
          if (editing.billing_expr !== null && editing.billing_expr !== undefined) {
            body.billing_expr = null
          }
        }
        // 策略缺省表示保持原值，因此关闭时段（或切到表达式）必须显式 null。
        if (parsedStrategy !== null) {
          body.strategy = parsedStrategy
        } else if (
          editing.has_strategy === true ||
          (editing.strategy !== null && editing.strategy !== undefined)
        ) {
          body.strategy = null
        }
        if (status !== editing.status) body.status = status
        await apiClient.PATCH('/api/model-pricing/{model}', {
          params: { path: { model: editing.model } },
          body,
        })
        push({ tone: 'success', message: t('admin.pricing.saved') })
      }
      await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing-detail'] })
      // 模型模式/价格变化会影响分组覆盖价的展示（mode/source_status），
      // 删除则会级联删除覆盖价；模型选择器也缓存了 mode/status。
      await queryClient.invalidateQueries({ queryKey: ['admin-pricing-models'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-pricing-candidates'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
      onSaved()
      onOpenChange(false)
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 创建 409 几乎总是 model 唯一键；更新 409 是 updated_at 乐观锁。
        if (editing === undefined || error.fields?.includes('model') === true) {
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
        let strategyField = false
        for (const field of error.fields) {
          if (field === 'billing_expr') {
            next.expr = t('admin.pricing.expression_invalid')
          } else if (field.startsWith('strategy')) {
            const mapped = mapStrategyField(field)
            if (mapped === null) {
              strategyField = true
            } else {
              next[mapped.key] = t(mapped.messageKey)
            }
          } else if (field === 'model') {
            next.model =
              editing === undefined
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
        if (strategyField) {
          setFormError(t('admin.pricing.strategy_server_rejected'))
        } else if (Object.keys(next).length === 0) {
          setFormError(t('admin.pricing.save_error'))
        }
      } else {
        setFormError(t('admin.pricing.save_error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  const effectivePrompt =
    (parsed.values.prompt ?? 0) * localMatch.multiplier
  const effectiveCompletion =
    (parsed.values.completion ?? 0) * localMatch.multiplier
  const timeRuleLabel = !timeConfig.enabled
    ? t('admin.pricing.summary_time_disabled')
    : localMatch.window === ''
      ? t('admin.pricing.preview_rule_default')
      : localMatch.window

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      widthClassName="max-w-[1100px]"
      title={
        editing !== undefined
          ? t('admin.pricing.edit_title', { model: editing.model })
          : t('admin.pricing.create_title')
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button
            type="submit"
            form="model-pricing-form"
            disabled={submitting || detailLoading || detailFailed}
          >
            {submitting
              ? editing !== undefined
                ? t('admin.pricing.saving')
                : t('admin.pricing.creating')
              : t('admin.pricing.save')}
          </Button>
        </>
      }
    >
      <form
        id="model-pricing-form"
        className="flex flex-col gap-4 lg:flex-row lg:items-start"
        onSubmit={(event) => void save(event)}
        noValidate
      >
        <div className="flex min-w-0 flex-1 flex-col gap-4">
          {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
          {detailLoading ? (
            <p className="text-xs text-ink-3" aria-busy="true">
              {t('admin.pricing.strategy_loading')}
            </p>
          ) : null}
          {detailFailed ? (
            <div className="flex flex-col gap-2" role="alert">
              <FormAlert>{t('admin.pricing.strategy_load_error')}</FormAlert>
              <Button
                variant="secondary"
                className="h-8 self-start px-2 text-[13px]"
                onClick={() => void detailQuery.refetch()}
              >
                {t('common.action.retry')}
              </Button>
            </div>
          ) : null}

          {mode === 'create' ? (
            <ModelCombobox
              value={model}
              error={fieldErrors.model}
              onChange={(next) => {
                setModel(next)
                if (existing !== null && next.trim() !== existing.model) {
                  setExisting(null)
                }
                setFieldErrors((current) => withoutKey(current, 'model'))
              }}
              onSelectCandidate={(candidate) => {
                void selectCandidate(candidate)
              }}
            />
          ) : (
            <div className="flex flex-col gap-1.5">
              <span className="text-[13px] font-medium text-ink-1">
                {t('admin.pricing.model')}
              </span>
              <span className="font-mono text-sm text-ink-1">{editing?.model ?? model}</span>
              <span className="text-xs text-ink-3">
                {editing?.has_strategy === true
                  ? t('admin.pricing.strategy_kind_peak_offpeak')
                  : t('admin.pricing.strategy_kind_flat')}
              </span>
            </div>
          )}

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
            <>
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

              <TimeWindowEditor
                value={timeConfig}
                errors={{ ...fieldErrors, ...strategyErrors }}
                onChange={changeTimeConfig}
              />

              <PricingBand
                config={timeConfig}
                selected={previewAt}
                onSelect={setPreviewAt}
              />

              <StrategySummary
                mode={pricingMode}
                timeEnabled={timeConfig.enabled}
                timeRuleLabel={timeRuleLabel}
                multiplier={localMatch.multiplier}
                prompt={parsed.values.prompt}
                completion={parsed.values.completion}
                cacheRead={parsed.values.cacheRead}
                cacheWrite={parsed.values.cacheWrite}
                effectivePrompt={effectivePrompt}
                effectiveCompletion={effectiveCompletion}
              />

              {pricingMode === 'per_token' ? (
                <details className="rounded-lg border border-border-hairline bg-surface-card p-3">
                  <summary className="cursor-pointer text-sm font-semibold text-ink-1">
                    {t('admin.pricing.tiers_title')}
                  </summary>
                  <p className="mt-2 text-xs text-ink-3">
                    {t('admin.pricing.tiers_phase2')}
                  </p>
                </details>
              ) : null}
            </>
          )}

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
        </div>

        <aside className="flex w-full shrink-0 flex-col gap-3 lg:sticky lg:top-0 lg:w-[360px]">
          {pricingMode === 'tiered_expr' ? (
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
          ) : (
            <StrategyPreview
              mode={pricingMode}
              prices={{
                prompt: parsed.values.prompt,
                completion: parsed.values.completion,
                cacheRead: parsed.values.cacheRead,
                cacheWrite: parsed.values.cacheWrite,
              }}
              config={timeConfig}
              at={previewAt}
              onAtChange={setPreviewAt}
              quotaPerUnit={factor}
            />
          )}
        </aside>
      </form>
    </Sheet>
  )
}

function StrategySummary({
  mode,
  timeEnabled,
  timeRuleLabel,
  multiplier,
  prompt,
  completion,
  cacheRead,
  cacheWrite,
  effectivePrompt,
  effectiveCompletion,
}: {
  mode: PricingMode
  timeEnabled: boolean
  timeRuleLabel: string
  multiplier: number
  prompt: number | null
  completion: number | null
  cacheRead: number | null
  cacheWrite: number | null
  effectivePrompt: number
  effectiveCompletion: number
}) {
  const { t } = useI18n()
  const base =
    mode === 'per_request'
      ? t('admin.pricing.summary_per_request', { prompt: prompt ?? 0 })
      : t('admin.pricing.summary_base', {
          prompt: prompt ?? 0,
          completion: completion ?? 0,
          cacheRead: cacheRead ?? 0,
          cacheWrite: cacheWrite ?? 0,
        })

  return (
    <section
      aria-label={t('admin.pricing.summary_title')}
      className="flex flex-col gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-3"
    >
      <h3 className="text-sm font-semibold text-ink-1">
        {t('admin.pricing.summary_title')}
      </h3>
      <p className="text-xs text-ink-3">{t('admin.pricing.summary_note')}</p>
      <dl className="mt-1 flex flex-col gap-1 text-xs text-ink-2">
        <div className="flex justify-between gap-3">
          <dt>{t('admin.pricing.summary_base_label')}</dt>
          <dd className="text-right font-mono tabular-nums text-ink-1">{base}</dd>
        </div>
        <div className="flex justify-between gap-3">
          <dt>{t('admin.pricing.summary_time_label')}</dt>
          <dd className="text-right font-mono tabular-nums text-ink-1">
            {timeEnabled
              ? t('admin.pricing.summary_time_value', {
                  rule: timeRuleLabel,
                  multiplier: formatMultiplierPercent(multiplier),
                })
              : timeRuleLabel}
          </dd>
        </div>
        {mode === 'per_token' ? (
          <div className="flex justify-between gap-3">
            <dt>{t('admin.pricing.summary_effective_label')}</dt>
            <dd className="text-right font-mono tabular-nums text-ink-1">
              {t('admin.pricing.summary_effective_value', {
                prompt: effectivePrompt,
                completion: effectiveCompletion,
              })}
            </dd>
          </div>
        ) : null}
      </dl>
    </section>
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

function withoutKeys(
  record: Record<string, string>,
  predicate: (key: string) => boolean,
): Record<string, string> {
  const next: Record<string, string> = {}
  for (const [key, value] of Object.entries(record)) {
    if (!predicate(key)) next[key] = value
  }
  return next
}

function isStrategyErrorKey(key: string): boolean {
  return (
    key === 'timezone' ||
    key === 'defaultMultiplier' ||
    key.startsWith('windows.')
  )
}

function translateStrategyErrors(
  errors: Record<string, boolean>,
  t: (key: string) => string,
): Record<string, string> {
  const out: Record<string, string> = {}
  for (const key of Object.keys(errors)) {
    if (key === 'timezone') {
      out.timezone = t('admin.pricing.time_timezone_invalid')
    } else if (key === 'defaultMultiplier') {
      out.defaultMultiplier = t('admin.pricing.time_multiplier_invalid')
    } else if (key.endsWith('.start')) {
      out[key] = t('admin.pricing.time_start_invalid')
    } else if (key.endsWith('.end')) {
      out[key] = t('admin.pricing.time_end_invalid')
    } else if (key.endsWith('.multiplier')) {
      out[key] = t('admin.pricing.time_multiplier_invalid')
    } else {
      out[key] = t('admin.pricing.time_invalid')
    }
  }
  return out
}

/**
 * 把服务端的 `strategy.time.windows[0].start` 这类路径映射回编辑器字段。
 * 无法定位到具体控件时返回 null，由调用方退化成表单级提示。
 */
function mapStrategyField(
  field: string,
): { key: string; messageKey: string } | null {
  if (!field.startsWith('strategy.time.')) return null
  const path = field
    .slice('strategy.time.'.length)
    .replace(/\[(\d+)\]/g, '.$1')
  if (path === 'timezone') {
    return { key: 'timezone', messageKey: 'admin.pricing.time_timezone_invalid' }
  }
  if (path === 'default_multiplier') {
    return {
      key: 'defaultMultiplier',
      messageKey: 'admin.pricing.time_multiplier_invalid',
    }
  }
  if (/^windows\.\d+\.start$/.test(path)) {
    return { key: path, messageKey: 'admin.pricing.time_start_invalid' }
  }
  if (/^windows\.\d+\.end$/.test(path)) {
    return { key: path, messageKey: 'admin.pricing.time_end_invalid' }
  }
  if (/^windows\.\d+\.multiplier$/.test(path)) {
    return { key: path, messageKey: 'admin.pricing.time_multiplier_invalid' }
  }
  if (/^windows\.\d+\.days$/.test(path)) {
    return { key: path, messageKey: 'admin.pricing.time_invalid' }
  }
  return null
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
