import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  listUserNotifications,
  NOTIFICATION_BADGE_CAP,
  NOTIFICATION_COUNT_PAGE_SIZE,
} from './api'
import { NotificationSheet } from './notification-sheet'
import { notificationsListQueryKey } from './query-keys'

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
 * 未读计数没有专用端点，只能读取一页通知并按 `read_at === null` 计数。
 * 这里取服务端允许的最大页长 100；超过部分显示 `99+`，并在 aria-live
 * 文本里保持同样口径（不谎报精确数）。探测失败完全静默：通知入口本身
 * 仍可打开面板查看错误与重试，绝不让顶栏出现错误横幅。
 */
export function NotificationBell({
  signedIn = true,
  onNavigate,
}: NotificationBellProps) {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)

  const countQuery = useQuery({
    queryKey: notificationsListQueryKey(1, NOTIFICATION_COUNT_PAGE_SIZE),
    queryFn: ({ signal }) =>
      listUserNotifications(
        { p: 1, page_size: NOTIFICATION_COUNT_PAGE_SIZE },
        signal,
      ),
    enabled: signedIn,
    // 通知不是实时流；30s 内复用（例如路由切换重挂载）已经足够新鲜。
    staleTime: 30_000,
    retry: false,
  })

  const unread =
    countQuery.data?.items.filter(
      (item) => item.read_at === null || item.read_at === undefined,
    ).length ?? 0
  const badge = unread > NOTIFICATION_BADGE_CAP ? `${NOTIFICATION_BADGE_CAP}+` : String(unread)
  const label =
    unread > 0
      ? t('common.notifications.bell_label', { count: unread })
      : t('common.notifications.bell_label_none')

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
            className="absolute -right-1 -top-1 inline-flex min-w-4 items-center justify-center rounded-full bg-danger px-1 text-[10px] font-semibold leading-4 text-on-brand"
          >
            {badge}
          </span>
        ) : null}
      </button>
      {/*
       * 计数变化通过独立 live region 播报。徽章本身 aria-hidden：否则数字
       * 会被读屏读成没有上下文的「2」，而按钮名已含完整句子。
       */}
      <span role="status" aria-live="polite" aria-atomic="true" className="sr-only">
        {unread > 0
          ? t('common.notifications.unread_count', { count: unread })
          : t('common.notifications.unread_count_none')}
      </span>
      <NotificationSheet
        open={open}
        onOpenChange={setOpen}
        onNavigate={onNavigate}
      />
    </>
  )
}
