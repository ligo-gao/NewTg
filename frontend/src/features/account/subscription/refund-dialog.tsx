import { useEffect, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'

import { refundError, type SubscriptionInlineError } from './errors'
import { formatSubscriptionTime } from './format'
import { isRefundEligible } from './support'
import { requestSubscriptionRefund } from './subscription-api'
import type { SubscriptionRefundRequestView, UserSubscription } from './types'

export const REFUND_REASON_MAX = 200

export interface RefundDialogProps {
  /** `null` closes the dialog. */
  subscription: UserSubscription | null
  offline: boolean
  onOpenChange: (open: boolean) => void
  onRequested: () => void
}

/**
 * 退款申请弹窗（S3；`POST /api/subscriptions/{id}/refund-request`）。
 *
 * 申请只写订单标记，**不调用渠道也不动钱包**——实际退款由管理员审批。
 * 因此成功态必须说清「已提交申请」，不能说「已退款」（这是资金诚信问题）。
 * 重复申请由服务端幂等处理，返回首次的申请时刻与理由。
 */
export function RefundDialog({
  subscription,
  offline,
  onOpenChange,
  onRequested,
}: RefundDialogProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const [reason, setReason] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<SubscriptionInlineError | null>(null)
  const [result, setResult] = useState<SubscriptionRefundRequestView | null>(null)

  const subscriptionId = subscription?.id ?? null
  useEffect(() => {
    setReason('')
    setSubmitting(false)
    setError(null)
    setResult(null)
  }, [subscriptionId])

  if (subscription === null) return null

  const eligible = isRefundEligible(subscription)
  const trimmed = reason.trim()
  const reasonValid = trimmed.length >= 1 && trimmed.length <= REFUND_REASON_MAX

  async function submit(): Promise<void> {
    if (submitting || offline || subscription === null || !eligible) return
    if (!reasonValid) {
      setError({ key: 'account.subscription.refund.error_invalid_reason' })
      return
    }
    setSubmitting(true)
    setError(null)
    try {
      const response = await requestSubscriptionRefund(subscription.id, trimmed)
      setResult(response)
      onRequested()
      push({
        tone: 'success',
        message: t('account.subscription.refund.success_title'),
      })
    } catch (caught) {
      setError(refundError(caught))
    } finally {
      setSubmitting(false)
    }
  }

  const footer = result !== null ? (
    <Button onClick={() => onOpenChange(false)}>
      {t('account.subscription.action.done')}
    </Button>
  ) : (
    <>
      <Button variant="ghost" disabled={submitting} onClick={() => onOpenChange(false)}>
        {t('account.subscription.action.cancel')}
      </Button>
      <Button
        disabled={offline || submitting || !eligible || !reasonValid}
        onClick={() => void submit()}
      >
        {submitting
          ? t('account.subscription.refund.submitting')
          : t('account.subscription.refund.submit')}
      </Button>
    </>
  )

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!submitting) onOpenChange(next)
      }}
      title={t('account.subscription.refund.title')}
      description={t('account.subscription.refund.description')}
      dismissible={!submitting}
      panelClassName="max-w-[480px]"
      footer={footer}
    >
      {result !== null ? (
        <div aria-live="polite" className="flex flex-col gap-2 text-sm">
          <p className="text-[15px] font-semibold text-success">
            {t('account.subscription.refund.success_title')}
          </p>
          <p className="text-ink-2">
            {t('account.subscription.refund.success_body', {
              time: formatSubscriptionTime(result.requested_at, locale),
            })}
          </p>
          <p className="text-ink-2">
            {t('account.subscription.refund.success_reason', {
              reason: result.reason,
            })}
          </p>
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          <p className="text-sm text-ink-2">
            {t('account.subscription.refund.reason_hint')}
          </p>
          <label className="flex flex-col gap-1 text-[13px] text-ink-2">
            <span>{t('account.subscription.refund.reason')}</span>
            <textarea
              value={reason}
              maxLength={REFUND_REASON_MAX}
              disabled={submitting || !eligible}
              onChange={(event) => setReason(event.target.value)}
              className="min-h-24 rounded-lg border border-border-default bg-surface-page px-3 py-2 text-sm text-ink-1 disabled:opacity-50"
              aria-describedby="refund-reason-count"
            />
          </label>
          <span id="refund-reason-count" className="text-xs text-ink-3 tabular-nums">
            {t('account.subscription.refund.reason_count', {
              count: reason.length,
              max: REFUND_REASON_MAX,
            })}
          </span>
          {!eligible ? (
            <p role="status" className="text-sm text-warning">
              {t('account.subscription.refund.ineligible')}
            </p>
          ) : null}
        </div>
      )}

      {error !== null ? (
        <p role="alert" className="mt-3 text-sm text-danger">
          {t(error.key, error.values)}
        </p>
      ) : null}
    </Dialog>
  )
}
