import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Sheet } from '@/shared/ui/base/sheet'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import {
  WALLET_STATUS_BADGE_VARIANTS,
  WALLET_TYPE_TAG_VARIANTS,
  statusLabelKey,
  typeLabelKey,
} from './labels'
import type { WalletTransactionDetail } from './types'

export interface TransactionDetailSheetProps {
  transactionId: number | null
  onOpenChange: (open: boolean) => void
  /** reversal 行跳转到被修正流水；父组件复用同一抽屉切换 id。 */
  onOpenCorrects?: (transactionId: number) => void
}

/**
 * 流水详情抽屉（F-03-03）。数据由服务端一次组装（R-M3-04）：usage/order/
 * redemption/adjustment/segments/corrects 全部来自同一个 detail 响应，
 * 前端不串多个端点，也不会撞上 /api/usage-logs 的 24h/90d 窗口。
 */
export function TransactionDetailSheet({
  transactionId,
  onOpenChange,
  onOpenCorrects,
}: TransactionDetailSheetProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)

  const detailQuery = useQuery({
    queryKey: ['wallet', 'transaction', transactionId],
    enabled: transactionId !== null,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/wallet/transactions/{id}', {
          params: { path: { id: transactionId as number } },
        }),
      ),
  })

  const detail: WalletTransactionDetail | undefined = detailQuery.data
  const notFound =
    detailQuery.error instanceof ApiError && detailQuery.error.kind === 'not_found'

  function copyTransactionId(): void {
    if (detail === undefined) return
    void navigator.clipboard?.writeText(String(detail.id)).catch(() => undefined)
    push({ tone: 'success', message: t('account.wallet.detail_copied') })
  }

  // 金额方向独立于颜色（色盲友好）：支出永远 U+2212，收入永远 +。
  const amountPrefix = detail !== undefined && detail.amount < 0 ? '−' : '+'
  const amountClass =
    detail !== undefined && detail.amount < 0 ? 'text-amount-out' : 'text-amount-in'

  return (
    <Sheet
      open={transactionId !== null}
      onOpenChange={onOpenChange}
      title={t('account.wallet.detail_title')}
      footer={
        <div className="flex items-center justify-end gap-2">
          <Button
            variant="secondary"
            disabled={detail === undefined}
            onClick={copyTransactionId}
          >
            {t('account.wallet.detail_copy')}
          </Button>
          <Button variant="primary" onClick={() => onOpenChange(false)}>
            {t('account.wallet.detail_close')}
          </Button>
        </div>
      }
    >
      {transactionId === null ? null : notFound ? (
        <p role="status" className="text-sm text-ink-2">
          {t('account.wallet.detail_not_found')}
        </p>
      ) : detailQuery.isPending ? (
        <p className="text-sm text-ink-2">{t('account.wallet.detail_loading')}</p>
      ) : detailQuery.isError || detail === undefined ? (
        <div className="flex items-center gap-3 text-sm text-ink-2">
          <span role="alert">{t('account.wallet.detail_error')}</span>
          <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
            {t('account.wallet.retry')}
          </Button>
        </div>
      ) : (
        <div className="flex flex-col gap-5">
          <header className="flex items-center justify-between gap-3">
            <p className={`text-[28px] font-semibold tabular-nums ${amountClass}`}>
              {amountPrefix}
              {formatCredits(Math.abs(detail.amount), locale)}
            </p>
            <StatusBadge variant={WALLET_STATUS_BADGE_VARIANTS[detail.status]}>
              {t(statusLabelKey(detail.status))}
            </StatusBadge>
          </header>

          <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
            <div className="flex flex-col gap-1">
              <dt className="text-xs text-ink-2">{t('account.wallet.detail_type')}</dt>
              <dd>
                <Tag variant={WALLET_TYPE_TAG_VARIANTS[detail.type]}>
                  {t(typeLabelKey(detail.type))}
                </Tag>
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-xs text-ink-2">{t('account.wallet.detail_time')}</dt>
              <dd className="text-ink-1">
                <time dateTime={detail.created_at}>
                  {new Date(detail.created_at).toLocaleString(locale)}
                </time>
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-xs text-ink-2">{t('account.wallet.detail_balance')}</dt>
              <dd className="tabular-nums text-ink-1">
                {formatCredits(detail.balance_after, locale)}
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-xs text-ink-2">{t('account.wallet.detail_status')}</dt>
              <dd className="text-ink-1">{t(statusLabelKey(detail.status))}</dd>
            </div>
          </dl>

          {detail.reference_type !== null && detail.reference_id !== null ? (
            <div className="flex flex-col gap-1 text-sm">
              <span className="text-xs text-ink-2">
                {t('account.wallet.detail_reference')}
              </span>
              <span className="break-all font-mono text-[13px] text-ink-1">
                {detail.reference_type} · {detail.reference_id}
              </span>
            </div>
          ) : null}

          {detail.note !== null ? (
            <section className="flex flex-col gap-1">
              <h3 className="text-sm font-medium text-ink-1">
                {t('account.wallet.detail_note')}
              </h3>
              <p className="text-sm text-ink-2">{detail.note}</p>
            </section>
          ) : null}

          {detail.reference_type === 'usage_log' ? (
            <section className="flex flex-col gap-2">
              <h3 className="text-sm font-medium text-ink-1">
                {t('account.wallet.detail_usage')}
              </h3>
              {detail.usage === null ? (
                <p className="text-sm text-ink-2">
                  {t('account.wallet.detail_usage_missing')}
                </p>
              ) : (
                <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
                  <div className="flex flex-col gap-1">
                    <dt className="text-xs text-ink-2">
                      {t('account.wallet.detail_usage_model')}
                    </dt>
                    <dd className="text-ink-1">{detail.usage.model}</dd>
                  </div>
                  <div className="flex flex-col gap-1">
                    <dt className="text-xs text-ink-2">
                      {t('account.wallet.detail_usage_tokens')}
                    </dt>
                    <dd className="tabular-nums text-ink-1">
                      {t('account.wallet.detail_usage_tokens_value', {
                        prompt: formatCredits(detail.usage.prompt_tokens, locale),
                        completion: formatCredits(
                          detail.usage.completion_tokens,
                          locale,
                        ),
                      })}
                    </dd>
                  </div>
                  <div className="flex flex-col gap-1">
                    <dt className="text-xs text-ink-2">
                      {t('account.wallet.detail_usage_cost')}
                    </dt>
                    <dd className="tabular-nums text-ink-1">
                      {formatCredits(detail.usage.cost, locale)}
                    </dd>
                  </div>
                  {detail.usage.pre_deducted !== null ? (
                    <div className="flex flex-col gap-1">
                      <dt className="text-xs text-ink-2">
                        {t('account.wallet.detail_segments_title')}
                      </dt>
                      <dd className="tabular-nums text-ink-1">
                        {formatCredits(detail.usage.pre_deducted, locale)}
                        {detail.usage.refunded !== null
                          ? ` / ${formatCredits(detail.usage.refunded, locale)}`
                          : ''}
                      </dd>
                    </div>
                  ) : null}
                </dl>
              )}
              {detail.segments !== null ? (
                <p className="text-sm text-ink-1">
                  {t('account.wallet.detail_segments', {
                    held: formatCredits(detail.segments.held, locale),
                    charged: formatCredits(detail.segments.charged, locale),
                    refunded: formatCredits(detail.segments.refunded, locale),
                  })}
                </p>
              ) : null}
            </section>
          ) : null}

          {detail.order !== null ? (
            <section className="flex flex-col gap-2">
              <h3 className="text-sm font-medium text-ink-1">
                {t('account.wallet.detail_order')}
              </h3>
              <p className="font-mono text-[13px] text-ink-1">#{detail.order.id}</p>
              <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
                <div className="flex flex-col gap-1">
                  <dt className="text-xs text-ink-2">
                    {t('account.wallet.detail_order_status')}
                  </dt>
                  <dd className="text-ink-1">{detail.order.status}</dd>
                </div>
                <div className="flex flex-col gap-1">
                  <dt className="text-xs text-ink-2">
                    {t('account.wallet.detail_order_amount')}
                  </dt>
                  <dd className="tabular-nums text-ink-1">
                    {detail.order.amount} {detail.order.currency}
                  </dd>
                </div>
              </dl>
              <a
                href="/orders"
                className="text-[13px] font-medium text-brand hover:text-brand-hover"
              >
                {t('account.wallet.detail_order_link')}
              </a>
            </section>
          ) : null}

          {detail.redemption !== null ? (
            <section className="flex flex-col gap-2">
              <h3 className="text-sm font-medium text-ink-1">
                {t('account.wallet.detail_redemption')}
              </h3>
              <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
                <div className="flex flex-col gap-1">
                  <dt className="text-xs text-ink-2">
                    {t('account.wallet.detail_redemption_batch')}
                  </dt>
                  <dd className="text-ink-1">
                    {detail.redemption.batch_name ?? '—'}
                  </dd>
                </div>
                <div className="flex flex-col gap-1">
                  <dt className="text-xs text-ink-2">
                    {t('account.wallet.detail_redemption_credits')}
                  </dt>
                  <dd className="tabular-nums text-ink-1">
                    {formatCredits(detail.redemption.credits, locale)}
                  </dd>
                </div>
              </dl>
            </section>
          ) : null}

          {detail.adjustment !== null ? (
            <section className="flex flex-col gap-1">
              <h3 className="text-sm font-medium text-ink-1">
                {t('account.wallet.detail_adjustment')}
              </h3>
              <p className="text-sm text-ink-2">
                {t('account.wallet.detail_adjustment_mode')}:{' '}
                {t(`account.wallet.adjustment_${detail.adjustment.mode}`)}
              </p>
            </section>
          ) : null}

          {detail.type === 'reversal' ? (
            <section
              role="alert"
              className="flex flex-col gap-2 rounded-lg border border-dashed border-accent-reversal p-3 text-sm text-accent-reversal"
            >
              <p>{t('account.wallet.reversal_warning')}</p>
              {detail.corrects_transaction_id !== null && onOpenCorrects ? (
                <button
                  type="button"
                  className="self-start text-[13px] font-medium underline"
                  onClick={() => {
                    onOpenCorrects(detail.corrects_transaction_id as number)
                  }}
                >
                  {t('account.wallet.reversal_link', {
                    id: detail.corrects_transaction_id,
                  })}
                </button>
              ) : null}
            </section>
          ) : null}
        </div>
      )}
    </Sheet>
  )
}
