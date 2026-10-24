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

import { centsToYuanInput, parseYuanToCents } from './format'
import { useOnlineStatus } from './use-online'

type SubscriptionPlan = components['schemas']['SubscriptionPlan']
type SubscriptionPlanInput = components['schemas']['SubscriptionPlanInput']
type PlanStatus = SubscriptionPlanInput['status']

/**
 * billing-08 §2.1 的稳定键字符集（与 backend `planKeyPattern` 一致）：
 * 小写字母/数字开头，只允许小写字母、数字与连字符。
 */
const PLAN_KEY_PATTERN = /^[a-z0-9][a-z0-9-]{0,63}$/

export interface SubscriptionPlanDrawerProps {
  open: boolean
  mode: 'create' | 'edit'
  plan?: SubscriptionPlan
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

interface FieldErrors {
  plan_key?: string
  title?: string
  description?: string
  period_days?: string
  price_amount?: string
  currency?: string
  grant_credits?: string
  grace_days?: string
  concurrency_limit?: string
  model_scope?: string
  status?: string
  sort_order?: string
}

function withoutKey(errors: FieldErrors, key: keyof FieldErrors): FieldErrors {
  const next = { ...errors }
  delete next[key]
  return next
}

/** Trims and parses an integer; null for empty/invalid input. */
function parseIntOrNull(value: string): number | null {
  const trimmed = value.trim()
  if (trimmed === '') return null
  if (!/^-?\d+$/.test(trimmed)) return null
  const parsed = Number(trimmed)
  return Number.isSafeInteger(parsed) ? parsed : null
}

/**
 * 套餐创建/编辑抽屉（billing-08 §16.2 F-H-01，480 宽）。
 *
 * `plan_key` 是稳定外部标识（§2.1「改名不改键」），只在创建时可填；编辑
 * 时以只读形式展示并原样提交（契约把该字段列为必填）。已购实例持有购买
 * 瞬间的快照，因此这里的任何修改都不追溯既有订阅。
 */
export function SubscriptionPlanDrawer({
  open,
  mode,
  plan,
  onOpenChange,
  onSaved,
}: SubscriptionPlanDrawerProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const offline = !useOnlineStatus()

  const [planKey, setPlanKey] = useState('')
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  const [periodDays, setPeriodDays] = useState('30')
  const [priceYuan, setPriceYuan] = useState('0.00')
  const [grantCredits, setGrantCredits] = useState('0')
  const [resetOnRenew, setResetOnRenew] = useState(true)
  const [allowWalletOverflow, setAllowWalletOverflow] = useState(true)
  // `model_scope` has three distinct contract states, so one array cannot
  // carry it: null = all models, [] = no model is admissible, [..] = allowlist.
  // The explicit switch keeps null and [] from collapsing into each other.
  const [allModels, setAllModels] = useState(true)
  const [modelScope, setModelScope] = useState<string[]>([])
  const [graceDays, setGraceDays] = useState('0')
  const [concurrencyLimit, setConcurrencyLimit] = useState('')
  const [status, setStatus] = useState<PlanStatus>('enabled')
  const [sortOrder, setSortOrder] = useState('0')
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setPlanKey(plan?.plan_key ?? '')
    setTitle(plan?.title ?? '')
    setDescription(plan?.description ?? '')
    setPeriodDays(String(plan?.period_days ?? 30))
    setPriceYuan(centsToYuanInput(plan?.price_amount ?? 0))
    setGrantCredits(String(plan?.grant_credits ?? 0))
    setResetOnRenew(plan?.reset_on_renew ?? true)
    setAllowWalletOverflow(plan?.allow_wallet_overflow ?? true)
    // Only an explicit null/omitted scope means "all models"; an empty array
    // is a real (if unusual) selection and must stay in list mode when edited.
    setAllModels(plan?.model_scope === null || plan?.model_scope === undefined)
    setModelScope(plan?.model_scope ?? [])
    setGraceDays(String(plan?.grace_days ?? 0))
    setConcurrencyLimit(
      plan?.concurrency_limit === null || plan?.concurrency_limit === undefined
        ? ''
        : String(plan.concurrency_limit),
    )
    setStatus(plan?.status ?? 'enabled')
    setSortOrder(String(plan?.sort_order ?? 0))
    setFieldErrors({})
    setFormError(undefined)
    setSubmitting(false)
  }, [open, plan])

  /**
   * 客户端校验与契约 422 同口径（plan_key 字符集、title 1–64、
   * description ≤255、period_days ≥1、价格/额度/宽限非负、并发 null 或 ≥1）。
   * 服务端仍是最终裁判；这里只避免把明显无效的输入发出去。
   */
  function collect(): SubscriptionPlanInput | null {
    const errors: FieldErrors = {}
    const trimmedKey = planKey.trim()
    if (mode === 'create' && !PLAN_KEY_PATTERN.test(trimmedKey)) {
      errors.plan_key = t('admin.subscriptions.drawer.plan_key_invalid')
    }
    const trimmedTitle = title.trim()
    if (trimmedTitle.length === 0 || trimmedTitle.length > 64) {
      errors.title = t('admin.subscriptions.drawer.field_invalid_title')
    }
    const trimmedDescription = description.trim()
    if (trimmedDescription.length > 255) {
      errors.description = t('admin.subscriptions.drawer.field_invalid_description')
    }
    const period = parseIntOrNull(periodDays)
    if (period === null || period < 1) {
      errors.period_days = t('admin.subscriptions.drawer.field_invalid_period_days')
    }
    const priceCents = parseYuanToCents(priceYuan)
    if (priceCents === null) {
      errors.price_amount = t('admin.subscriptions.drawer.field_invalid_price_amount')
    }
    const grant = parseIntOrNull(grantCredits)
    if (grant === null || grant < 0) {
      errors.grant_credits = t('admin.subscriptions.drawer.field_invalid_grant_credits')
    }
    const grace = parseIntOrNull(graceDays)
    if (grace === null || grace < 0) {
      errors.grace_days = t('admin.subscriptions.drawer.field_invalid_grace_days')
    }
    const concurrency = parseIntOrNull(concurrencyLimit)
    if (concurrencyLimit.trim() !== '' && (concurrency === null || concurrency < 1)) {
      errors.concurrency_limit = t(
        'admin.subscriptions.drawer.field_invalid_concurrency_limit',
      )
    }
    const sort = parseIntOrNull(sortOrder)
    if (sort === null) {
      errors.sort_order = t('admin.subscriptions.drawer.field_invalid_sort_order')
    }
    const scope = Array.from(
      new Set(modelScope.map((entry) => entry.trim()).filter((entry) => entry !== '')),
    )
    if (!allModels && scope.some((entry) => entry.length > 128)) {
      errors.model_scope = t('admin.subscriptions.drawer.field_invalid_model_scope')
    }

    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return null

    // The switch is authoritative. On = null (all models). Off = the list
    // exactly as entered, including [] — silently promoting an empty list to
    // null would widen a deliberately empty allowlist into "every model".
    return {
      plan_key: trimmedKey,
      title: trimmedTitle,
      description: trimmedDescription === '' ? null : trimmedDescription,
      period_days: period ?? 1,
      price_amount: priceCents ?? 0,
      currency: 'CNY',
      grant_credits: grant ?? 0,
      reset_on_renew: resetOnRenew,
      allow_wallet_overflow: allowWalletOverflow,
      model_scope: allModels ? null : scope,
      concurrency_limit: concurrencyLimit.trim() === '' ? null : concurrency,
      grace_days: grace ?? 0,
      status,
      sort_order: sort ?? 0,
    }
  }

  function serverFieldErrors(error: ApiError): FieldErrors {
    const next: FieldErrors = {}
    for (const field of error.fields ?? []) {
      switch (field) {
        case 'plan_key':
          next.plan_key = t('admin.subscriptions.drawer.field_invalid_plan_key')
          break
        case 'title':
          next.title = t('admin.subscriptions.drawer.field_invalid_title')
          break
        case 'description':
          next.description = t('admin.subscriptions.drawer.field_invalid_description')
          break
        case 'period_days':
          next.period_days = t('admin.subscriptions.drawer.field_invalid_period_days')
          break
        case 'price_amount':
          next.price_amount = t('admin.subscriptions.drawer.field_invalid_price_amount')
          break
        case 'currency':
          next.currency = t('admin.subscriptions.drawer.field_invalid_currency')
          break
        case 'grant_credits':
          next.grant_credits = t('admin.subscriptions.drawer.field_invalid_grant_credits')
          break
        case 'grace_days':
          next.grace_days = t('admin.subscriptions.drawer.field_invalid_grace_days')
          break
        case 'concurrency_limit':
          next.concurrency_limit = t(
            'admin.subscriptions.drawer.field_invalid_concurrency_limit',
          )
          break
        case 'model_scope':
          next.model_scope = t('admin.subscriptions.drawer.field_invalid_model_scope')
          break
        case 'status':
          next.status = t('admin.subscriptions.drawer.field_invalid_status')
          break
        case 'sort_order':
          next.sort_order = t('admin.subscriptions.drawer.field_invalid_sort_order')
          break
        default:
          break
      }
    }
    return next
  }

  async function submit(body: SubscriptionPlanInput): Promise<void> {
    setFormError(undefined)
    setSubmitting(true)
    try {
      if (mode === 'create') {
        await apiClient.POST('/api/subscription-plans', { body })
        push({ tone: 'success', message: t('admin.subscriptions.drawer.create_success') })
      } else if (plan !== undefined) {
        // No expected_updated_at exists yet, so a full PUT is last-write-wins.
        // Sending status only when the operator actually changed it narrows
        // that window: the server keeps its current status on omission
        // (`subscriptionPlanPayload.Status` is a pointer). See ledger G-DOM-41.
        if (body.status === plan.status) {
          delete (body as Partial<SubscriptionPlanInput>).status
        }
        await apiClient.PUT('/api/subscription-plans/{id}', {
          params: { path: { id: plan.id } },
          body,
        })
        push({ tone: 'success', message: t('admin.subscriptions.drawer.update_success') })
      }
      await queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
      onSaved()
      onOpenChange(false)
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 创建时 409 只有一个来源：plan_key 唯一键。
        setFormError(t('admin.subscriptions.drawer.plan_key_conflict'))
        await queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
      } else if (error instanceof ApiError && error.fields !== undefined) {
        const next = serverFieldErrors(error)
        setFieldErrors(next)
        if (Object.keys(next).length === 0) {
          setFormError(t('admin.subscriptions.drawer.save_error'))
        }
      } else if (error instanceof ApiError && error.kind === 'rate_limited') {
        setFormError(
          error.retry_after === undefined
            ? t('admin.subscriptions.rate_limited_generic')
            : t('admin.subscriptions.rate_limited', { seconds: error.retry_after }),
        )
      } else if (error instanceof ApiError && error.kind === 'offline') {
        setFormError(t('admin.subscriptions.offline_notice'))
      } else {
        setFormError(t('admin.subscriptions.drawer.save_error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  function save(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    if (offline || submitting) return
    const body = collect()
    if (body === null) return
    void submit(body)
  }

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={
        mode === 'create'
          ? t('admin.subscriptions.drawer.create_title')
          : t('admin.subscriptions.drawer.edit_title')
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.cancel')}
          </Button>
          <Button
            type="submit"
            form="subscription-plan-form"
            disabled={submitting || offline}
          >
            {submitting
              ? t('admin.subscriptions.drawer.saving')
              : t('admin.subscriptions.drawer.save')}
          </Button>
        </>
      }
    >
      <form
        id="subscription-plan-form"
        className="flex flex-col gap-5"
        onSubmit={save}
        noValidate
      >
        {offline ? <FormAlert tone="warning">{t('admin.subscriptions.offline_notice')}</FormAlert> : null}
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <section className="flex flex-col gap-3">
          <h3 className="text-meta font-semibold text-ink-1">
            {t('admin.subscriptions.drawer.section_basic')}
          </h3>
          <TextField
            label={t('admin.subscriptions.drawer.plan_key')}
            value={planKey}
            maxLength={64}
            disabled={mode === 'edit'}
            hint={
              mode === 'edit'
                ? t('admin.subscriptions.drawer.plan_key_locked_hint')
                : t('admin.subscriptions.drawer.plan_key_hint')
            }
            error={fieldErrors.plan_key}
            onChange={(event) => {
              setPlanKey(event.target.value)
              setFieldErrors((current) => withoutKey(current, 'plan_key'))
            }}
          />
          <TextField
            label={t('admin.subscriptions.drawer.title_field')}
            value={title}
            maxLength={64}
            error={fieldErrors.title}
            onChange={(event) => {
              setTitle(event.target.value)
              setFieldErrors((current) => withoutKey(current, 'title'))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label
              htmlFor="subscription-plan-description"
              className="text-meta font-medium text-ink-1"
            >
              {t('admin.subscriptions.drawer.description')}
            </label>
            <textarea
              id="subscription-plan-description"
              value={description}
              maxLength={255}
              rows={3}
              aria-invalid={fieldErrors.description !== undefined ? true : undefined}
              aria-describedby={
                fieldErrors.description !== undefined
                  ? 'subscription-plan-description-error'
                  : undefined
              }
              className={[
                'w-full rounded-lg border bg-surface-card px-3 py-2 text-body text-ink-1',
                'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                fieldErrors.description !== undefined
                  ? 'border-danger'
                  : 'border-border-default',
              ].join(' ')}
              onChange={(event) => {
                setDescription(event.target.value)
                setFieldErrors((current) => withoutKey(current, 'description'))
              }}
            />
            {fieldErrors.description !== undefined ? (
              <p id="subscription-plan-description-error" className="text-meta text-danger">
                {fieldErrors.description}
              </p>
            ) : null}
          </div>
        </section>

        <section className="flex flex-col gap-3 border-t border-border-hairline pt-4">
          <h3 className="text-meta font-semibold text-ink-1">
            {t('admin.subscriptions.drawer.section_period')}
          </h3>
          <div className="grid grid-cols-2 gap-3">
            <TextField
              label={t('admin.subscriptions.drawer.period_days')}
              inputMode="numeric"
              value={periodDays}
              error={fieldErrors.period_days}
              onChange={(event) => {
                setPeriodDays(event.target.value)
                setFieldErrors((current) => withoutKey(current, 'period_days'))
              }}
            />
            <TextField
              label={t('admin.subscriptions.drawer.price_amount')}
              inputMode="decimal"
              value={priceYuan}
              error={fieldErrors.price_amount}
              hint={t('admin.subscriptions.drawer.price_hint')}
              onChange={(event) => {
                setPriceYuan(event.target.value)
                setFieldErrors((current) => withoutKey(current, 'price_amount'))
              }}
            />
          </div>
          <TextField
            label={t('admin.subscriptions.drawer.currency')}
            value="CNY"
            readOnly
            disabled
            hint={t('admin.subscriptions.drawer.currency_hint')}
          />
          <TextField
            label={t('admin.subscriptions.drawer.grant_credits')}
            inputMode="numeric"
            value={grantCredits}
            error={fieldErrors.grant_credits}
            hint={t('admin.subscriptions.drawer.grant_credits_hint')}
            onChange={(event) => {
              setGrantCredits(event.target.value)
              setFieldErrors((current) => withoutKey(current, 'grant_credits'))
            }}
          />
          <label className="flex items-start gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              className="mt-0.5 size-4 accent-brand"
              checked={resetOnRenew}
              onChange={(event) => setResetOnRenew(event.target.checked)}
            />
            <span>
              {t('admin.subscriptions.drawer.reset_on_renew')}
              <span className="block text-meta text-ink-3">
                {t('admin.subscriptions.drawer.reset_on_renew_hint')}
              </span>
            </span>
          </label>
          <label className="flex items-start gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              className="mt-0.5 size-4 accent-brand"
              checked={allowWalletOverflow}
              onChange={(event) => setAllowWalletOverflow(event.target.checked)}
            />
            <span>
              {t('admin.subscriptions.drawer.allow_wallet_overflow')}
              <span className="block text-meta text-ink-3">
                {t('admin.subscriptions.drawer.allow_wallet_overflow_hint')}
              </span>
            </span>
          </label>
        </section>

        <section className="flex flex-col gap-3 border-t border-border-hairline pt-4">
          <h3 className="text-meta font-semibold text-ink-1">
            {t('admin.subscriptions.drawer.section_scope')}
          </h3>
          <div className="flex flex-col gap-1.5">
            <span className="text-meta font-medium text-ink-1">
              {t('admin.subscriptions.drawer.model_scope')}
            </span>
            <p className="text-meta text-ink-3">
              {t('admin.subscriptions.drawer.model_scope_hint')}
            </p>
            <label className="flex items-start gap-2 text-body text-ink-1">
              <input
                type="checkbox"
                className="mt-0.5 size-4 accent-brand"
                checked={allModels}
                onChange={(event) => {
                  setAllModels(event.target.checked)
                  setFieldErrors((current) => withoutKey(current, 'model_scope'))
                }}
              />
              <span>
                {t('admin.subscriptions.drawer.all_models_toggle')}
                <span className="block text-meta text-ink-3">
                  {t('admin.subscriptions.drawer.all_models_toggle_hint')}
                </span>
              </span>
            </label>
            {!allModels ? (
              <>
                {modelScope.map((entry, index) => (
              <div key={index} className="flex items-center gap-2">
                <input
                  aria-label={t('admin.subscriptions.drawer.model_scope_row', {
                    index: index + 1,
                  })}
                  value={entry}
                  maxLength={128}
                  placeholder={t('admin.subscriptions.drawer.model_scope_placeholder')}
                  className="h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  onChange={(event) => {
                    const value = event.target.value
                    setModelScope((current) =>
                      current.map((item, itemIndex) =>
                        itemIndex === index ? value : item,
                      ),
                    )
                    setFieldErrors((current) => withoutKey(current, 'model_scope'))
                  }}
                />
                <Button
                  type="button"
                  variant="ghost"
                  className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                  aria-label={t('admin.subscriptions.drawer.remove_model', {
                    index: index + 1,
                  })}
                  onClick={() => {
                    setModelScope((current) =>
                      current.filter((_item, itemIndex) => itemIndex !== index),
                    )
                  }}
                >
                  {t('admin.subscriptions.drawer.remove_model_label')}
                </Button>
              </div>
                ))}
                <div>
                  <Button
                    type="button"
                    variant="secondary"
                    className="h-8 px-2 text-meta"
                    onClick={() => setModelScope((current) => [...current, ''])}
                  >
                    {t('admin.subscriptions.drawer.add_model')}
                  </Button>
                </div>
              </>
            ) : null}
            {fieldErrors.model_scope !== undefined ? (
              <p className="text-meta text-danger">{fieldErrors.model_scope}</p>
            ) : null}
          </div>
          <div className="grid grid-cols-2 gap-3">
            <TextField
              label={t('admin.subscriptions.drawer.grace_days')}
              inputMode="numeric"
              value={graceDays}
              error={fieldErrors.grace_days}
              hint={t('admin.subscriptions.drawer.grace_days_hint')}
              onChange={(event) => {
                setGraceDays(event.target.value)
                setFieldErrors((current) => withoutKey(current, 'grace_days'))
              }}
            />
            <TextField
              label={t('admin.subscriptions.drawer.concurrency_limit')}
              inputMode="numeric"
              value={concurrencyLimit}
              disabled
              error={fieldErrors.concurrency_limit}
              hint={t('admin.subscriptions.drawer.concurrency_limit_hint')}
              onChange={(event) => {
                setConcurrencyLimit(event.target.value)
                setFieldErrors((current) => withoutKey(current, 'concurrency_limit'))
              }}
            />
          </div>
        </section>

        <section className="flex flex-col gap-3 border-t border-border-hairline pt-4">
          <h3 className="text-meta font-semibold text-ink-1">
            {t('admin.subscriptions.drawer.section_status')}
          </h3>
          <div className="flex flex-col gap-1.5">
            <label
              htmlFor="subscription-plan-status"
              className="text-meta font-medium text-ink-1"
            >
              {t('admin.subscriptions.drawer.status_field')}
            </label>
            <select
              id="subscription-plan-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={status}
              onChange={(event) => setStatus(event.target.value as PlanStatus)}
            >
              <option value="enabled">{t('admin.subscriptions.status_enabled')}</option>
              <option value="disabled">{t('admin.subscriptions.status_disabled')}</option>
            </select>
            {fieldErrors.status !== undefined ? (
              <p className="text-meta text-danger">{fieldErrors.status}</p>
            ) : null}
          </div>
          <TextField
            label={t('admin.subscriptions.drawer.sort_order')}
            inputMode="numeric"
            value={sortOrder}
            error={fieldErrors.sort_order}
            onChange={(event) => {
              setSortOrder(event.target.value)
              setFieldErrors((current) => withoutKey(current, 'sort_order'))
            }}
          />
        </section>
      </form>
    </Sheet>
  )
}
