import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { meQueryOptions } from '@/lib/session'
import { formatCredits } from '@/shared/lib/format'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

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
 * 数据源 `GET /api/users/{id}/subscriptions` 需要 `users:read`；无权限时在
 * 弹窗内展示 403 文案而不是假装空列表。`order_id` 以脱敏订单号 `#id` 呈现
 * （§15.4）。
 *
 * 管理侧取消（C3/R-08/R-47）：只有持 `users:update` 的管理员看到行内
 * 「立即取消」。权限判定用 `GET /api/me` 的生效权限（M6 R-17），前端闸门
 * 只是 UX——服务端仍是最终权威（403 时按错误提示，不放宽）。
 *
 * 立即取消的语义（R-48）与用户侧周期末取消**不同**：它把 end_at 提前到
 * 「现在」，实例进入快照宽限期（grace_days），宽限期内仍可继续使用已购
 * 额度，宽限结束才到期；已购额度不退款。确认框必须写明这一点，否则管理
 * 员会误以为「立即取消 = 立刻停用」。
 */
export function UserSubscriptionsDialog({ user, onClose }: UserSubscriptionsDialogProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [cancelTarget, setCancelTarget] = useState<AdminSubscription | null>(null)

  const me = useQuery(meQueryOptions())
  // FAIL-CLOSED：会话未定/权限为空时不渲染写操作（用户侧 role=user 的
  // permissions 恒为空数组，即使误挂管理员页面也不会出现取消按钮）。
  const canCancel =
    me.data?.role === 'admin' &&
    (me.data.permissions.includes('*') || me.data.permissions.includes('users:update'))

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

  const cancelNow = useMutation({
    mutationFn: async (target: AdminSubscription) => {
      await apiClient.POST('/api/users/{id}/subscriptions/{subscription_id}/cancel', {
        params: {
          path: { id: target.user_id, subscription_id: target.id },
        },
        body: { immediate: true },
      })
    },
    // 取消不是幂等可重试的「临时失败」：409 是确定性拒绝，重试只会把
    // 提示延后；429 也不该在客户端放大压力。
    retry: false,
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.subscriptions.user.cancel_now_success') })
      setCancelTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-user-subscriptions', user?.id] })
    },
    onError: (error) => {
      push({ tone: 'error', message: cancelError(error, t) })
      if (
        error instanceof ApiError &&
        (error.kind === 'conflict' || error.kind === 'not_found')
      ) {
        // 行状态已被别处改变（例如已到期）：关闭确认框并重查，避免
        // 继续展示过期状态。
        setCancelTarget(null)
        void queryClient.invalidateQueries({
          queryKey: ['admin-user-subscriptions', user?.id],
        })
      }
    },
  })

  const items = query.data?.items ?? []
  const error = query.error

  return (
    <>
    <Dialog
      open={user !== null}
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={t('admin.subscriptions.user.title', { username: user?.username ?? '' })}
      description={t('admin.subscriptions.user.description')}
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
            {error instanceof ApiError && error.kind === 'offline'
              ? t('admin.subscriptions.offline_read_error')
              : error instanceof ApiError && error.kind === 'forbidden'
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
          <table className="w-full min-w-[1040px] text-sm">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_status')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_plan')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_start')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_end')}
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
                  {t('admin.subscriptions.user.column_auto_renew')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_cancel_at_end')}
                </th>
                <th scope="col" className="px-3 py-2 font-medium">
                  {t('admin.subscriptions.user.column_order')}
                </th>
                {canCancel ? (
                  <th scope="col" className="px-3 py-2 text-right font-medium">
                    {t('admin.subscriptions.user.column_actions')}
                  </th>
                ) : null}
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
                  <td className="px-3 py-2 text-xs text-ink-2">
                    {formatDateTime(item.start_at, locale)}
                  </td>
                  <td className="px-3 py-2 text-xs text-ink-2">
                    {formatDateTime(item.end_at, locale)}
                  </td>
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
                  <td className="px-3 py-2">
                    <StatusBadge variant={item.auto_renew ? 'success' : 'neutral'}>
                      {item.auto_renew
                        ? t('admin.subscriptions.user.auto_renew_on')
                        : t('admin.subscriptions.user.auto_renew_off')}
                    </StatusBadge>
                  </td>
                  <td className="px-3 py-2">
                    <StatusBadge
                      variant={item.cancel_at_period_end ? 'warning' : 'neutral'}
                    >
                      {item.cancel_at_period_end
                        ? t('admin.subscriptions.user.cancel_at_end_yes')
                        : t('admin.subscriptions.user.cancel_at_end_no')}
                    </StatusBadge>
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-ink-2">
                    {item.order_id === null || item.order_id === undefined
                      ? '—'
                      : t('admin.subscriptions.user.order_ref', { id: item.order_id })}
                  </td>
                  {canCancel ? (
                    <td className="px-3 py-2 text-right">
                      {/* 只有 active 可取消：终态实例不渲染按钮（服务端同样
                          409，前端只是不给出注定失败的操作）。 */}
                      {item.status === 'active' ? (
                        <Button
                          variant="danger"
                          className="h-8 px-2 text-[13px]"
                          onClick={() => setCancelTarget(item)}
                        >
                          {t('admin.subscriptions.user.cancel_now')}
                        </Button>
                      ) : null}
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
          {query.data !== undefined && query.data.total > items.length ? (
            <p className="mt-2 text-xs text-ink-3">
              {t('admin.subscriptions.user.cap_notice', { count: items.length })}
            </p>
          ) : null}
        </div>
      )}
    </Dialog>
    {/* 确认框是兄弟节点而不是嵌套 Dialog：嵌套会让两个 focus-trap 争抢焦点，
        且 aria-modal 语义不明确（与 ModelsTab/RedemptionCodesPage 同构）。 */}
    <Dialog
      open={cancelTarget !== null}
        onOpenChange={(open) => {
          if (!open) setCancelTarget(null)
        }}
        role="alertdialog"
        title={t('admin.subscriptions.user.cancel_now_title')}
        description={t('admin.subscriptions.user.cancel_now_body', {
          days: cancelTarget?.grace_days ?? 0,
        })}
        footer={
          <>
            <Button variant="secondary" onClick={() => setCancelTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={cancelNow.isPending}
              onClick={() => {
                if (cancelTarget !== null) cancelNow.mutate(cancelTarget)
              }}
            >
              {t('admin.subscriptions.user.cancel_now_confirm')}
            </Button>
          </>
        }
      />
    </>
  )
}

function cancelError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'conflict') return t('admin.subscriptions.user.cancel_now_conflict')
    if (error.kind === 'forbidden') return t('admin.subscriptions.user.cancel_now_forbidden')
    if (error.kind === 'rate_limited') return t('admin.subscriptions.rate_limited_generic')
    if (error.kind === 'offline') return t('admin.subscriptions.offline_read_error')
  }
  return t('admin.subscriptions.user.cancel_now_error')
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
