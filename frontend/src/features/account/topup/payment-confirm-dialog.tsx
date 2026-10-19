import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { centsToCredits, formatYuan } from './money'
import type { CreateOrderResponse, TopupProvider } from './types'

export interface PaymentConfirmDialogProps {
  open: boolean
  /** Amount in cents, as selected on the recharge card. */
  amount: number
  provider: TopupProvider
  quotaPerUnit?: number
  onOpenChange: (open: boolean) => void
  onCreated: (response: CreateOrderResponse) => void
}

function newKey(): string {
  return globalThis.crypto.randomUUID()
}

/**
 * Payment confirmation (F-04-02, F-04-10).
 *
 * One Idempotency-Key is minted when the dialog opens and kept for every retry
 * of that same dialog: a failed network call or 5xx must replay to the same
 * order, not create a second one. Closing the dialog discards the key; the next
 * open is a fresh user intent and mints a new UUID v4.
 */
export function PaymentConfirmDialog({
  open,
  amount,
  provider,
  quotaPerUnit = 100,
  onOpenChange,
  onCreated,
}: PaymentConfirmDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const keyRef = useRef<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    keyRef.current = open ? newKey() : null
    if (!open) setSubmitting(false)
  }, [open])

  const credits = centsToCredits(amount, quotaPerUnit)
  const providerLabel = t(
    provider === 'stripe'
      ? 'account.orders.provider_stripe'
      : 'account.orders.provider_alipay',
  )

  async function confirm(): Promise<void> {
    if (submitting) return
    const key = keyRef.current ?? (keyRef.current = newKey())
    setSubmitting(true)
    try {
      const response = requireData(
        await apiClient.POST('/api/orders', {
          params: { header: { 'Idempotency-Key': key } },
          body: { amount, currency: 'CNY', provider },
        }),
      )
      onCreated(response)
    } catch {
      push({ tone: 'error', message: t('account.topup.create_failed') })
      // Keep the dialog open and the key intact so a retry is idempotent.
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!submitting) onOpenChange(next)
      }}
      title={t('account.topup.confirm_title')}
      dismissible={!submitting}
      footer={
        <>
          <Button
            variant="ghost"
            disabled={submitting}
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('account.topup.cancel')}
          </Button>
          <Button disabled={submitting} onClick={() => void confirm()}>
            {submitting ? t('account.topup.creating') : t('account.topup.confirm_pay')}
          </Button>
        </>
      }
    >
      <dl className="flex flex-col gap-2">
        <LedgerRow label={t('account.topup.you_pay')} value={formatYuan(amount)} />
        <LedgerRow
          label={t('account.topup.credits_received')}
          value={formatCredits(credits)}
        />
        <div className="flex items-center justify-between gap-4 text-sm">
          <dt className="text-ink-2">{t('account.topup.payment_method')}</dt>
          <dd className="font-medium text-ink-1">{providerLabel}</dd>
        </div>
      </dl>
    </Dialog>
  )
}

function LedgerRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-end gap-2 text-sm">
      <dt className="text-ink-2">{label}</dt>
      <span
        aria-hidden="true"
        className="mb-1 flex-1 border-b border-dotted border-border-default"
      />
      <dd className="font-medium tabular-nums text-ink-1">{value}</dd>
    </div>
  )
}
