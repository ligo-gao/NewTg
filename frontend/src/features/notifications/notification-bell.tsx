import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  getUserNotificationUnreadCount,
  NOTIFICATION_BADGE_CAP,
} from './api'
import { NotificationSheet } from './notification-sheet'
import { notificationsUnreadCountQueryKey } from './query-keys'

export interface NotificationBellProps {
  /**
   * 会话已就绪后才允许发探测请求。路由守卫尚未确定身份时拉取只会得到
   * 401，也会给未登录首屏制造无意义的请求。
   */
  signedIn?: boolean
  onNavigate?: (href: string) => void
}

/**
 * 控制台顶栏铃铛（A5 站内通知入口）。
 *
 * 未读数直接来自 `GET /api/notifications/unread-count`：服务端按与列表相同
 * 的口径计数（含公告失效过滤），不再靠翻 `page_size=100` 一页去聚合，因此
 * 第 101 条以后的未读也不会漏计。徽章与 aria-live 仍统一封顶 `99+`，不谎报
 * 精确值。探测失败完全静默：通知入口本身仍可打开面板查看错误与重试，绝不
 * 让顶栏出现错误横幅。
 */
export function NotificationBell({
  signedIn = true,
  onNavigate,
}: NotificationBellProps) {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)

  const countQuery = useQuery({
    queryKey: notificationsUnreadCountQueryKey,
    queryFn: ({ signal }) => getUserNotificationUnreadCount(signal),
    enabled: signedIn,
    // 通知不是实时流；30s 内复用（例如路由切换重挂载）已经足够新鲜。
    staleTime: 30_000,
    retry: false,
  })

  const unread = countQuery.data?.unread ?? 0
  /*
   * 封顶口径只有一处真源：徽章与 aria-live 都用 `99+` 表示「至少 99 条」。
   * 服务端给出的是精确数，超过封顶值时不展示具体数字，避免读屏播报与徽章
   * 不一致。
   */
  const displayCount =
    unread > NOTIFICATION_BADGE_CAP
      ? `${NOTIFICATION_BADGE_CAP}+`
      : String(unread)
  const label =
    unread === 0
      ? t('common.notifications.bell_label_none')
      : unread === 1
        ? t('common.notifications.bell_label_one', { count: 1 })
        : t('common.notifications.bell_label', { count: displayCount })

  return (
    <>
      <button
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={label}
        onClick={() => setOpen(true)}
        className="relative inline-flex size-8 items-center justify-center rounded-lg border border-border-default bg-surface-page text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        <svg
          aria-hidden="true"
          viewBox="0 0 16 16"
          className="size-4"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
        >
          <path
            d="M8 2.25a3.25 3.25 0 0 0-3.25 3.25v2.06L3.5 9.75v.75h9v-.75L11.25 7.56V5.5A3.25 3.25 0 0 0 8 2.25Z"
            strokeLinejoin="round"
          />
          <path d="M6.5 12a1.5 1.5 0 0 0 3 0" strokeLinecap="round" />
        </svg>
        {unread > 0 ? (
          <span
            aria-hidden="true"
            className="absolute -right-1 -top-1 inline-flex min-w-4 items-center justify-center rounded-full bg-danger px-1 text-badge font-semibold leading-4 text-on-brand"
          >
            {displayCount}
          </span>
        ) : null}
      </button>
      {/*
       * 计数变化通过独立 live region 播报。徽章本身 aria-hidden：否则数字
       * 会被读屏读成没有上下文的「2」，而按钮名已含完整句子。
       */}
      <span role="status" aria-live="polite" aria-atomic="true" className="sr-only">
        {unread === 0
          ? t('common.notifications.unread_count_none')
          : unread === 1
            ? t('common.notifications.unread_count_one', { count: 1 })
            : t('common.notifications.unread_count', {
                count: displayCount,
              })}
      </span>
      <NotificationSheet
        open={open}
        onOpenChange={setOpen}
        onNavigate={onNavigate}
      />
    </>
  )
}
