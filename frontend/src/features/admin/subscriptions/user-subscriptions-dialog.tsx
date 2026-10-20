import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import { formatDateTime } from './format'

type AdminUser = components['schemas']['AdminUser']
type AdminSubscription = components['schemas']['AdminSubscription']
type SubscriptionStatus = AdminSubscription['status']

const STATUS_VARIANT: Record<SubscriptionStatus, StatusBadgeVariant> = {
  active: 'success',
  // Terminal instances are inert, not errors: neutral per modules-00 §4.6.4.
  expired: 'neutral',
  cancelled: 'neutral-outline',
}

export interface UserSubscriptionsDialogProps {
  /** `null` closes the dialog; the user is the row that opened it. */
  user: AdminUser | null
  onClose: () => void
}

/**
 * Manage Subscriptions（billing-08 §16.4 F-H-05）。
 *
 * 只读：管理侧「立即取消」端点尚未实现（登记 P2），因此这里不渲染任何写
 * 控件。数据源 `GET /api/users/{id}/subscriptions` 需要 `users:read`；无
 * 权限时在弹窗内展示 403 文案而不是假装空列表。`order_id` 以脱敏订单号
 * `#id` 呈现（§15.4）。
 */
export function UserSubscriptionsDialog({ user, onClose }: UserSubscriptionsDialogProps) {
  const { t, locale } = useI18n()

  const query = useQuery({
    queryKey: ['admin-user-subscriptions', user?.id],
    enabled: user !== null,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/users/{id}/subscriptions', {
          params: {
            path: { id: user?.id ?? 0 },
            // The dialog is a management overview, not a pager: show the
            // newest 100 (contract maximum) rather than hiding older active
            // instances behind a second page.
            query: { page_size: 100 },
          },
        }),
      ),
  })

  const items = query.data?.items ?? []
  const error = query.error

  return (
    <Dialog
      open={user !== null}
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={t('admin.subscriptions.user.title', { username: user?.username ?? '' })}
      description={t('admin.subscriptions.user.read_only')}
      panelClassName="!max-w-[720px]"
      footer={
        <Button variant="secondary" onClick={onClose}>
          {t('admin.subscriptions.user.close')}
        </Button>
      }
    >
      {query.isPending ? (
        <div className="space-y-2" aria-busy="true">
          {[0, 1, 2].map((index) => (
            <div key={index} className="h-12 animate-pulse rounded bg-surface-subtle" />
          ))}
        </div>
      ) : query.isError ? (
        <div className="flex items-center gap-3 text-sm text-ink-2">
          <span>
            {error instanceof ApiError && error.kind === 'forbidden'
              ? t('admin.subscriptions.user.forbidden')
              : error instanceof ApiError && error.kind === 'rate_limited'
                ? error.retry_after === undefined
                  ? t('admin.subscriptions.rate_limited_generic')
                  : t('admin.subscriptions.rate_limited', {
                      seconds: error.retry_after,
                    })
                : t('admin.subscriptions.user.error')}
          </span>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('common.action.retry')}
          </Button>
        </div>
      ) : items.length === 0 ? (
        <p className="text-sm text-ink-2">{t('admin.subscriptions.user.empty')}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_status')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_plan')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_period')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_quota')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_next_reset')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_end')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_order')}
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr
                  key={item.id}
                  className="border-b border-border-hairline last:border-0"
                >
                  <td className="px-3 py-2">
                    <StatusBadge variant={STATUS_VARIANT[item.status]}>
                      {t(`admin.subscriptions.user.status_${item.status}`)}
                    </StatusBadge>
                  </td>
                  <td className="px-3 py-2 text-ink-1">{item.plan_title_snapshot}</td>
                  <td className="px-3 py-2 text-ink-2">
                    {t('admin.subscriptions.period_days', { count: item.period_days })}
                  </td>
                  <td className="px-3 py-2">
                    <QuotaCell item={item} locale={locale} />
                  </td>
                  <td className="px-3 py-2 text-xs text-ink-2">
                    {item.next_reset_at === null || item.next_reset_at === undefined
                      ? '—'
                      : formatDateTime(item.next_reset_at, locale)}
                  </td>
                  <td className="px-3 py-2 text-xs text-ink-2">
                    {formatDateTime(item.end_at, locale)}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-ink-2">
                    {item.order_id === null || item.order_id === undefined
                      ? '—'
                      : t('admin.subscriptions.user.order_ref', { id: item.order_id })}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Dialog>
  )
}

function QuotaCell({
  item,
  locale,
}: {
  item: AdminSubscription
  locale: string
}) {
  const { t } = useI18n()

  // amount_total = 0 means unlimited (billing-08 §2.3); the server omits
  // amount_remain then, and the UI must not invent a denominator.
  if (item.amount_total === 0) {
    return (
      <span className="text-ink-1">{t('admin.subscriptions.unlimited')}</span>
    )
  }
  const used = Math.min(item.amount_used, item.amount_total)
  const percent = Math.round((used / item.amount_total) * 100)
  return (
    <div className="flex min-w-[120px] flex-col gap-1">
      <span className="font-mono text-xs tabular-nums text-ink-1">
        {t('admin.subscriptions.user.used_of', {
          used: formatCredits(item.amount_used, locale),
          total: formatCredits(item.amount_total, locale),
        })}
      </span>
      <span
        className="h-1.5 w-full overflow-hidden rounded-full bg-surface-subtle"
        aria-hidden="true"
      >
        <span
          className="block h-full rounded-full bg-brand"
          style={{ width: `${percent}%` }}
        />
      </span>
    </div>
  )
}
