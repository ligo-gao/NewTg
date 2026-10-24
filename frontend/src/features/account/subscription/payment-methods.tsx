import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CardElement, Elements, useElements, useStripe } from '@stripe/react-stripe-js'
import { loadStripe, type Stripe } from '@stripe/stripe-js'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import {
  createPaymentMethod,
  deletePaymentMethod,
  listPaymentMethods,
} from './subscription-api'
import { TOPUP_CONFIG_KEY } from './constants'
import { MY_SUBSCRIPTIONS_KEY } from './my-subscriptions'
import type { PaymentMethod } from './types'

export const PAYMENT_METHODS_KEY = ['payment-methods'] as const

export interface PaymentMethodsSectionProps {
  offline: boolean
}

/**
 * 支付令牌区（S4；P-14）。
 *
 * 令牌是 Stripe PaymentMethod id，卡号只在浏览器端提交给 Stripe；网关保存
 * 的是 AES-GCM 密文，读取响应只回 brand/last4/exp（零回显）。删除会
 * fail-closed 关闭引用它的自动续费（R-42），因此删除后同时失效订阅列表。
 */
export function PaymentMethodsSection({ offline }: PaymentMethodsSectionProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<string | null>(null)

  const methodsQuery = useQuery({
    queryKey: PAYMENT_METHODS_KEY,
    queryFn: async () => listPaymentMethods(),
  })

  async function handleDelete(method: PaymentMethod): Promise<void> {
    if (offline || deleting !== null) return
    setDeleting(method.id)
    try {
      await deletePaymentMethod(method.id)
      push({ tone: 'success', message: t('account.subscription.payment_methods.deleted') })
      // 删除可能关闭订阅的自动续费（R-42）：两个列表都必须重读。
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: PAYMENT_METHODS_KEY }),
        queryClient.invalidateQueries({ queryKey: MY_SUBSCRIPTIONS_KEY }),
      ])
    } catch {
      push({ tone: 'error', message: t('account.subscription.payment_methods.delete_error') })
    } finally {
      setDeleting(null)
    }
  }

  return (
    <section
      aria-labelledby="payment-methods-title"
      className="rounded-xl border border-border-hairline bg-surface-card p-5"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="payment-methods-title" className="text-card-title text-ink-1">
            {t('account.subscription.payment_methods.title')}
          </h2>
          <p className="mt-1 text-meta text-ink-2">
            {t('account.subscription.payment_methods.subtitle')}
          </p>
        </div>
        <Button variant="secondary" onClick={() => setAdding(true)} disabled={offline}>
          {t('account.subscription.payment_methods.add')}
        </Button>
      </div>

      {methodsQuery.isPending ? (
        <div
          role="status"
          aria-label={t('account.subscription.payment_methods.loading')}
          aria-busy="true"
          className="mt-4 h-14 animate-pulse rounded-lg bg-surface-subtle"
        />
      ) : methodsQuery.isError ? (
        <div className="mt-4 flex items-center gap-3 text-body text-ink-2">
          <span>{t('account.subscription.payment_methods.error')}</span>
          <Button variant="secondary" onClick={() => void methodsQuery.refetch()}>
            {t('account.subscription.payment_methods.retry')}
          </Button>
        </div>
      ) : (methodsQuery.data?.items.length ?? 0) === 0 ? (
        <p className="mt-4 text-body text-ink-2">
          {t('account.subscription.payment_methods.empty')}
        </p>
      ) : (
        <ul className="mt-4 flex flex-col gap-2">
          {methodsQuery.data?.items.map((method) => (
            <li
              key={method.id}
              className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border-hairline bg-surface-page px-3 py-2"
            >
              <div className="flex flex-col">
                <span className="text-body font-medium text-ink-1">
                  {t('account.subscription.payment_methods.card_ending', {
                    brand: method.brand,
                    last4: method.last4,
                  })}
                </span>
                <span className="text-meta text-ink-2">
                  {t('account.subscription.payment_methods.expires', {
                    month: String(method.exp_month).padStart(2, '0'),
                    year: method.exp_year,
                  })}
                  {method.status === 'invalid'
                    ? ` · ${t('account.subscription.payment_methods.invalid')}`
                    : ''}
                </span>
              </div>
              <Button
                variant="secondary"
                disabled={offline || deleting !== null}
                onClick={() => void handleDelete(method)}
              >
                {deleting === method.id
                  ? t('account.subscription.payment_methods.deleting')
                  : t('account.subscription.payment_methods.delete')}
              </Button>
            </li>
          ))}
        </ul>
      )}

      <AddPaymentMethodDialog
        open={adding}
        onOpenChange={setAdding}
        onSaved={() => {
          push({ tone: 'success', message: t('account.subscription.payment_methods.saved') })
          void queryClient.invalidateQueries({ queryKey: PAYMENT_METHODS_KEY })
        }}
      />
    </section>
  )
}

interface AddPaymentMethodDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}

/**
 * 新增卡片对话框：Stripe Elements 在浏览器端创建 PaymentMethod，网关只收
 * `pm_...` 并回查展示字段。publishable key 来自公开配置 `GET /api/topup/config`；
 * 未配置时不渲染表单（fail closed），而不是让用户填卡后才发现无法保存。
 */
function AddPaymentMethodDialog({ open, onOpenChange, onSaved }: AddPaymentMethodDialogProps) {
  const { t } = useI18n()
  const [stripe, setStripe] = useState<Stripe | null>(null)
  const [loadFailed, setLoadFailed] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState(false)

  const configQuery = useQuery({
    queryKey: TOPUP_CONFIG_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/topup/config')),
    enabled: open,
  })
  const publishableKey = configQuery.data?.providers?.stripe?.publishable_key ?? ''

  useEffect(() => {
    if (!open || publishableKey === '') return
    let cancelled = false
    loadStripe(publishableKey)
      .then((instance) => {
        if (!cancelled) {
          setStripe(instance)
          setLoadFailed(false)
        }
      })
      .catch(() => {
        if (!cancelled) setLoadFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [open, publishableKey])

  const footer = (
    <>
      <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={submitting}>
        {t('account.subscription.payment_methods.cancel')}
      </Button>
      <Button type="submit" form="add-payment-method-form" disabled={submitting || stripe === null}>
        {submitting
          ? t('account.subscription.payment_methods.saving')
          : t('account.subscription.payment_methods.save')}
      </Button>
    </>
  )

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!submitting) {
          onOpenChange(next)
          setError(false)
        }
      }}
      title={t('account.subscription.payment_methods.dialog_title')}
      description={t('account.subscription.payment_methods.dialog_hint')}
      dismissible={!submitting}
      panelClassName="max-w-[480px]"
      footer={footer}
    >
      {configQuery.isSuccess && publishableKey === '' ? (
        <p className="text-body text-ink-2">
          {t('account.subscription.payment_methods.unavailable')}
        </p>
      ) : loadFailed || configQuery.isError ? (
        <p role="alert" className="text-body text-danger">
          {t('account.subscription.payment_methods.unavailable')}
        </p>
      ) : stripe === null ? (
        <div
          role="status"
          aria-busy="true"
          className="h-14 animate-pulse rounded-lg bg-surface-subtle"
        />
      ) : (
        <Elements stripe={stripe}>
          <AddCardForm
            submitting={submitting}
            error={error}
            onSubmit={async (token) => {
              setSubmitting(true)
              setError(false)
              try {
                await createPaymentMethod(token)
                setSubmitting(false)
                onSaved()
                onOpenChange(false)
              } catch {
                setSubmitting(false)
                setError(true)
              }
            }}
          />
        </Elements>
      )}
    </Dialog>
  )
}

function AddCardForm({
  submitting,
  error,
  onSubmit,
}: {
  submitting: boolean
  error: boolean
  onSubmit: (token: string) => Promise<void>
}) {
  const { t } = useI18n()
  const stripe = useStripe()
  const elements = useElements()
  async function handleSubmit(event: React.FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (submitting || stripe === null || elements === null) return
    const card = elements.getElement(CardElement)
    if (card === null) return
    const result = await stripe.createPaymentMethod({ type: 'card', card })
    if (result.error !== undefined || result.paymentMethod === undefined) {
      // 卡片校验错误留在 Elements；只有令牌创建失败才阻止保存。
      return
    }
    await onSubmit(result.paymentMethod.id)
  }

  return (
    <form id="add-payment-method-form" onSubmit={(e) => void handleSubmit(e)} className="flex flex-col gap-2">
      <span className="text-meta font-medium text-ink-1">
        {t('account.subscription.payment_methods.card_label')}
      </span>
      <div
        role="group"
        aria-label={t('account.subscription.payment_methods.card_label')}
        className="rounded-lg border border-border-hairline bg-surface-page px-3 py-3"
      >
        <CardElement options={{ hidePostalCode: true }} />
      </div>
      {error ? (
        <p role="alert" className="text-meta text-danger">
          {t('account.subscription.payment_methods.save_error')}
        </p>
      ) : null}
    </form>
  )
}
