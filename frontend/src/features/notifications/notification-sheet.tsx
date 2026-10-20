import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Sheet } from '@/shared/ui/base/sheet'
import { Button } from '@/shared/ui/button'

import {
  collectUnreadNotificationIds,
  markUserNotificationRead,
  listUserNotifications,
  NOTIFICATION_PAGE_SIZE,
  type UserNotification,
} from './api'
import {
  applyNotificationRead,
  NOTIFICATIONS_QUERY_PREFIX,
  notificationsListQueryKey,
} from './query-keys'

export interface NotificationSheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /**
   * 导航接缝。生产环境缺省为整页跳转（通知链接可能是站外绝对 URL）；
   * 测试可注入以断言目标，避免触碰 jsdom 未实现的 location.assign。
   */
  onNavigate?: (href: string) => void
}

function isUnread(item: UserNotification): boolean {
  return item.read_at === null || item.read_at === undefined
}

/**
 * 通知目标地址：服务端 payload 的 link_url 优先，其次回退到公告详情路径。
 *
 * 只接受站内单斜杠相对路径与 http(s) 绝对 URL；`javascript:`、协议相对
 * `//host` 等一律丢弃。link_url 由服务端公告发布时写入，当前实现指向站点
 * 根（D7b 公告详情页尚未落地），因此 fallback 才是「点公告看详情」的
 * 预期路径；通知载荷没有单独的公告 id，只能用投递 id 占位（已登记契约
 * 差异）。
 */
export function notificationHref(
  item: Pick<UserNotification, 'id' | 'link_url'>,
): string {
  const raw = item.link_url?.trim() ?? ''
  if (raw.startsWith('/') && !raw.startsWith('//')) return raw
  if (/^https?:\/\//i.test(raw)) return raw
  return `/announcements/${item.id}`
}

function formatAbsolute(value: string, locale: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(date)
}

function categoryLabel(item: UserNotification, t: TranslateFn): string {
  // 当前站内信只由 `system.announcement` 模板产生；其它类别未来出现时
  // 显示服务端的 category 原文，避免前端维护一份会漂移的枚举。
  if (
    item.template_key === 'system.announcement' ||
    item.category.includes('announcement')
  ) {
    return t('common.notifications.category_announcement')
  }
  return item.category
}

function describeError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'offline') {
      return t('common.notifications.error_offline')
    }
    if (error.kind === 'rate_limited') {
      // Retry-After 是服务端事实；缺失时给出一个明确的等待窗口，
      // 但不假装它是服务端下发的。
      return t('common.notifications.error_rate_limited', {
        seconds: error.retry_after ?? 60,
      })
    }
  }
  return t('common.notifications.error_generic')
}

/**
 * 右侧通知面板。挂载在控制台顶栏铃铛之下，只有 `open` 时才拉列表，
 * 因此登录后首屏只付一次「未读探测」的请求成本。
 */
export function NotificationSheet({
  open,
  onOpenChange,
  onNavigate,
}: NotificationSheetProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [markingAll, setMarkingAll] = useState(false)
  const [actionError, setActionError] = useState<'read' | 'all' | null>(null)

  // 每次打开都回到第一页：闭面板前翻到第 5 页、再打开却看到旧页会让
  // 用户以为新通知丢了。
  useEffect(() => {
    if (open) {
      setPage(1)
      setActionError(null)
    }
  }, [open])

  const query = useQuery({
    queryKey: notificationsListQueryKey(page, NOTIFICATION_PAGE_SIZE),
    queryFn: ({ signal }) =>
      listUserNotifications(
        { p: page, page_size: NOTIFICATION_PAGE_SIZE },
        signal,
      ),
    enabled: open,
    // 翻页时保留上一页内容，避免面板高度归零导致焦点环闪烁。
    placeholderData: keepPreviousData,
    staleTime: 15_000,
    retry: false,
  })

  const markRead = useMutation({
    mutationFn: markUserNotificationRead,
    onSuccess: (_data, id) => {
      // 先本地递减，再以服务端列表为准重取（保持幂等/他端变更一致）。
      applyNotificationRead(queryClient, id)
      void queryClient.invalidateQueries({
        queryKey: NOTIFICATIONS_QUERY_PREFIX,
      })
    },
  })

  const data = query.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / NOTIFICATION_PAGE_SIZE))

  async function openItem(item: UserNotification): Promise<void> {
    const href = notificationHref(item)
    setActionError(null)
    if (isUnread(item)) {
      setBusyId(item.id)
      try {
        await markRead.mutateAsync(item.id)
      } catch {
        // 已读标记失败不应剥夺用户查看内容的权利；服务端幂等，用户下次
        // 打开仍可重试。这里只在面板内提示，不阻塞跳转。
        setActionError('read')
      } finally {
        setBusyId(null)
      }
    }
    onOpenChange(false)
    if (onNavigate !== undefined) {
      onNavigate(href)
      return
    }
    if (typeof window !== 'undefined') window.location.assign(href)
  }

  async function markAllRead(): Promise<void> {
    setMarkingAll(true)
    setActionError(null)
    try {
      const ids = await collectUnreadNotificationIds()
      // 逐条调用：契约没有批量端点，且单条幂等，重复/并发重试安全。
      for (const id of ids) {
        await markUserNotificationRead(id)
      }
      await queryClient.invalidateQueries({
        queryKey: NOTIFICATIONS_QUERY_PREFIX,
      })
    } catch {
      setActionError('all')
      // 部分成功也要刷新，让 UI 反映真实剩余未读。
      void queryClient.invalidateQueries({
        queryKey: NOTIFICATIONS_QUERY_PREFIX,
      })
    } finally {
      setMarkingAll(false)
    }
  }

  const rateLimited =
    query.error instanceof ApiError && query.error.kind === 'rate_limited'
  const unreadOnPage = items.filter(isUnread).length

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={t('common.notifications.title')}
      description={t('common.notifications.description')}
      footer={
        <Button variant="secondary" onClick={() => onOpenChange(false)}>
          {t('common.action.close')}
        </Button>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs text-ink-3">
            {t('common.notifications.total', { count: total })}
          </p>
          {/*
           * 服务端无批量端点；按钮遍历全部页的未读并逐条标记。只在当前
           * 页有未读时也保持可用——其它页可能仍有未读，不能据此禁用。
           */}
          <Button
            variant="secondary"
            className="h-8 px-2.5 text-[13px]"
            onClick={() => void markAllRead()}
            disabled={markingAll || (total === 0 && unreadOnPage === 0)}
          >
            {markingAll
              ? t('common.notifications.marking_all')
              : t('common.notifications.mark_all')}
          </Button>
        </div>

        {actionError !== null ? (
          <p role="alert" className="text-sm text-danger">
            {actionError === 'read'
              ? t('common.notifications.mark_read_failed')
              : t('common.notifications.mark_all_failed')}
          </p>
        ) : null}

        {query.isPending ? (
          <p role="status" className="py-6 text-center text-sm text-ink-3">
            {t('common.notifications.loading')}
          </p>
        ) : query.isError ? (
          <div role="alert" className="flex flex-col items-start gap-2 py-4">
            <p className="text-sm text-ink-2">
              {describeError(query.error, t)}
            </p>
            <Button
              variant="secondary"
              className="h-8 px-3 text-[13px]"
              onClick={() => void query.refetch()}
              disabled={rateLimited}
            >
              {t('common.notifications.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <p role="status" className="py-10 text-center text-sm text-ink-3">
            {t('common.notifications.empty')}
          </p>
        ) : (
          <ul
            aria-busy={query.isFetching}
            aria-label={t('common.notifications.list_label')}
            className="flex flex-col gap-2"
          >
            {items.map((item) => {
              const unread = isUnread(item)
              return (
                <li key={item.id}>
                  <button
                    type="button"
                    onClick={() => void openItem(item)}
                    disabled={busyId === item.id}
                    className="flex w-full flex-col gap-1 rounded-lg border border-border-hairline bg-surface-page p-3 text-left hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-60"
                  >
                    <span className="flex items-start gap-2">
                      {unread ? (
                        <span
                          aria-hidden="true"
                          className="mt-1.5 size-2 shrink-0 rounded-full bg-brand"
                        />
                      ) : (
                        <span aria-hidden="true" className="mt-1.5 size-2 shrink-0" />
                      )}
                      <span className="min-w-0 flex-1">
                        <span className="block text-sm font-medium text-ink-1">
                          {item.title || t('common.notifications.untitled')}
                        </span>
                        <span className="mt-0.5 block text-xs text-ink-3">
                          <time
                            dateTime={item.created_at}
                            title={formatAbsolute(item.created_at, locale)}
                          >
                            {formatRelativeTime(item.created_at, locale)}
                          </time>
                          {' · '}
                          {categoryLabel(item, t)}
                          {unread ? (
                            <>
                              {' · '}
                              <span className="font-medium text-brand">
                                {t('common.notifications.unread')}
                              </span>
                            </>
                          ) : null}
                        </span>
                      </span>
                    </span>
                    {item.summary ? (
                      <span className="text-sm text-ink-2">{item.summary}</span>
                    ) : null}
                    {/*
                     * body 是按用户语言渲染后的 Markdown 文本。这里按纯文本
                     * 展示（whitespace-pre-wrap），不解析、不 innerHTML——
                     * 通知正文没有可依赖的 HTML 净化契约，纯文本是唯一安全的
                     * 呈现方式。
                     */}
                    {item.body ? (
                      <span className="block whitespace-pre-wrap break-words text-sm leading-5 text-ink-2">
                        {item.body}
                      </span>
                    ) : null}
                  </button>
                </li>
              )
            })}
          </ul>
        )}

        {data !== undefined && total > NOTIFICATION_PAGE_SIZE ? (
          <nav
            aria-label={t('common.notifications.pagination.label')}
            className="mt-1 flex items-center justify-between gap-2 border-t border-border-hairline pt-3"
          >
            <Button
              variant="ghost"
              className="h-8 px-2 text-[13px]"
              onClick={() => setPage((current) => Math.max(1, current - 1))}
              disabled={page <= 1 || query.isFetching}
            >
              {t('common.notifications.pagination.prev')}
            </Button>
            <p className="text-xs text-ink-3">
              {t('common.notifications.pagination.page', { page, pages })}
            </p>
            <Button
              variant="ghost"
              className="h-8 px-2 text-[13px]"
              onClick={() =>
                setPage((current) => Math.min(pages, current + 1))
              }
              disabled={page >= pages || query.isFetching}
            >
              {t('common.notifications.pagination.next')}
            </Button>
          </nav>
        ) : null}
      </div>
    </Sheet>
  )
}
