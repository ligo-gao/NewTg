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
  getUserNotificationUnreadCount,
  markAllUserNotificationsRead,
  markUserNotificationRead,
  listUserNotifications,
  NOTIFICATION_PAGE_SIZE,
  type UserNotification,
} from './api'
import {
  applyAllNotificationsRead,
  applyNotificationRead,
  NOTIFICATIONS_QUERY_PREFIX,
  notificationsListQueryKey,
  notificationsUnreadCountQueryKey,
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

// 站内路径解析用的固定哨兵 base：只用来判断「解析后主机有没有跑掉」，
// 永远不会被导航或展示。用 `.invalid`（RFC 2606 保留 TLD）保证它不可能
// 与真实站点重合。
const INTERNAL_BASE = 'https://internal.invalid'
const INTERNAL_BASE_ORIGIN = new URL(INTERNAL_BASE).origin

/**
 * 通知目标地址（D7b 起公告详情页已落地）。
 *
 * 优先级冻结为：
 *  1. 投递载荷自带的 `announcement_id`（A5b 写入，服务端作为
 *     `UserNotification.announcement_id` 下发）→ `/announcements/{id}`。
 *     这是唯一能保证落到**该公告**详情的来源；`link_url` 是发布时刻的
 *     快照，历史投递可能仍指向旧路径。
 *  2. `link_url`：只接受两种形态——绝对 http(s) URL，或站内深链。
 *     判定前先按 WHATWG URL 的规则剥离 TAB/LF/CR：解析器会在识别 scheme
 *     之前丢弃这三个字符，`/\t/evil.example` 因此会被解释成
 *     `//evil.example`（站外），任何「先看字面前两字符」的检查都会漏掉。
 *     归一后的站内候选用固定 base 做一次 `new URL(compact, base)` 并比对
 *     origin，一次覆盖 `//host`、`/\host`（反斜杠在特殊 scheme 中等价于
 *     斜杠）、空白注入与大小写：只要解析结果不是 base 的 origin，就说明
 *     浏览器会把它当成另一台主机，直接丢弃。`javascript:`、裸相对路径等
 *     一律丢弃。
 *  3. 两者都没有可用目标时返回 `null`——**不导航**。不再用投递 id 凭空
 *     拼 `/announcements/{id}`：那不是公告 id，只会落到 404。
 */
export function notificationHref(
  item: Pick<UserNotification, 'link_url' | 'announcement_id'>,
): string | null {
  const announcementId = item.announcement_id?.trim()
  if (announcementId !== undefined && announcementId !== '') {
    return `/announcements/${encodeURIComponent(announcementId)}`
  }
  const raw = item.link_url?.trim() ?? ''
  if (raw === '') return null

  // 与 WHATWG 解析器同口径归一：这三个字符在 URL 里被静默移除，若不先
  // 剥掉，`"/\t/evil.example"` 之类会以「单斜杠站内路径」的字面通过检查，
  // 浏览器却把它解析成 `//evil.example`。
  const compact = raw.replace(/[\t\n\r]/g, '')
  if (compact === '') return null

  // 绝对地址：仅 http(s)。校验后仍返回**归一后**的串，保持与服务端下发
  // 内容一致（不重建 URL、不补默认端口/尾斜杠）。
  if (/^https?:\/\//i.test(compact)) {
    try {
      const url = new URL(compact)
      return url.protocol === 'http:' || url.protocol === 'https:'
        ? compact
        : null
    } catch {
      return null
    }
  }

  // 站内路径必须是单斜杠开头；`//`、`/\`、`javascript:`、裸相对串都在
  // 这里被挡掉。固定 base 只用于解析，不会出现在返回值里。
  if (!compact.startsWith('/')) return null
  try {
    return new URL(compact, INTERNAL_BASE).origin === INTERNAL_BASE_ORIGIN
      ? compact
      : null
  } catch {
    return null
  }
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

function describeError(
  error: unknown,
  t: TranslateFn,
  retrySeconds: number | null,
): string {
  if (error instanceof ApiError) {
    if (error.kind === 'offline') {
      return t('common.notifications.error_offline')
    }
    if (error.kind === 'rate_limited') {
      /*
       * 倒计时只来自服务端 `Retry-After`：有值且仍大于 0 时展示剩余秒数，
       * 到期后改成「可重试」文案；没有该头就**不发明时长**，直接允许手动
       * 重试（旧实现的 `?? 60` 会让用户白等一分钟）。
       */
      if (retrySeconds !== null && retrySeconds > 0) {
        return t('common.notifications.error_rate_limited', {
          seconds: retrySeconds,
        })
      }
      return t('common.notifications.error_rate_limited_ready')
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

  /*
   * 未读计数：与铃铛共用同一个 query key，面板打开时通常直接命中缓存、
   * 不额外发请求。服务端按与列表相同的可见性口径计数，所以「全部已读」
   * 是否可用不再受首屏 100 条窗口限制。
   */
  const unreadCountQuery = useQuery({
    queryKey: notificationsUnreadCountQueryKey,
    queryFn: ({ signal }) => getUserNotificationUnreadCount(signal),
    enabled: open,
    staleTime: 30_000,
    retry: false,
  })

  /*
   * 429 倒计时：deadline 来自服务端 `retry_after`（缺失则保持 null），
   * `nowMs` 由 250ms 定时器推进以便每秒刷新剩余秒数。到期后定时器自行
   * 停止，重试按钮恢复可点；不自动重发请求（避免用户还在读错误时再次
   * 触发限流）。
   */
  const retryAfterSeconds =
    query.error instanceof ApiError && query.error.kind === 'rate_limited'
      ? query.error.retry_after
      : undefined
  const [retryDeadline, setRetryDeadline] = useState<number | null>(null)
  const [nowMs, setNowMs] = useState(() => Date.now())

  useEffect(() => {
    setNowMs(Date.now())
    setRetryDeadline(
      retryAfterSeconds === undefined
        ? null
        : Date.now() + retryAfterSeconds * 1000,
    )
    // `query.error` 每次失败都是新对象；同一秒数的新 429 也应重新计时。
  }, [query.error, retryAfterSeconds])

  useEffect(() => {
    if (retryDeadline === null) return undefined
    const timer = setInterval(() => {
      const current = Date.now()
      setNowMs(current)
      if (current >= retryDeadline) clearInterval(timer)
    }, 250)
    return () => clearInterval(timer)
  }, [retryDeadline])

  const retrySeconds =
    retryDeadline === null
      ? null
      : Math.max(0, Math.ceil((retryDeadline - nowMs) / 1000))

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
        /*
         * 已读失败必须可见：保留面板并显示行内错误，绝不悄悄关闭后跳走
         * （用户会以为已读成功，回来却发现仍是未读）。服务端幂等，用户
         * 可再次点击重试。
         */
        setActionError('read')
        return
      } finally {
        setBusyId(null)
      }
    }
    // 无可用目标（无 announcement_id、link_url 不是白名单形态）时不导航，
    // 也不关闭面板：不存在可展示的详情，关闭只会让点击看起来生效了。
    if (href === null) return
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
      // 服务端单条 SQL 集合置读：不存在部分成功，重复调用也安全。
      await markAllUserNotificationsRead()
      applyAllNotificationsRead(queryClient)
      await queryClient.invalidateQueries({
        queryKey: NOTIFICATIONS_QUERY_PREFIX,
      })
    } catch {
      setActionError('all')
      // 失败时仍刷新，让 UI 反映服务端真实剩余未读（可能 0 也可能未变）。
      void queryClient.invalidateQueries({
        queryKey: NOTIFICATIONS_QUERY_PREFIX,
      })
    } finally {
      setMarkingAll(false)
    }
  }

  const retryBlocked = retrySeconds !== null && retrySeconds > 0
  const unreadOnPage = items.filter(isUnread).length
  // 计数结果优先（覆盖全部通知，而不是只有当前页）；尚未就绪时退回当前页判断。
  const hasUnread =
    unreadCountQuery.data !== undefined
      ? unreadCountQuery.data.unread > 0
      : unreadOnPage > 0

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
            {t(
              total === 1
                ? 'common.notifications.total_one'
                : 'common.notifications.total',
              { count: total },
            )}
          </p>
          {/*
           * 服务端批量端点一次置读全部可见未读；只有确认未读数为 0 时才禁用。
           */}
          <Button
            variant="secondary"
            className="h-8 px-2.5 text-[13px]"
            onClick={() => void markAllRead()}
            disabled={markingAll || !hasUnread}
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
              {describeError(query.error, t, retrySeconds)}
            </p>
            <Button
              variant="secondary"
              className="h-8 px-3 text-[13px]"
              onClick={() => void query.refetch()}
              disabled={retryBlocked}
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
                     * 不渲染 `body`：它是按用户语言渲染后的 Markdown 原文，
                     * 直接当纯文本展示会把 `**加粗**`、`[查看详情](/)` 之类
                     * 标记字面量暴露给用户。站内信的可用摘要已由服务端的
                     * `summary`（剥链接/图片语法、收敛为纯文本）承担；详情
                     * 在公告页呈现，面板只做「标题 + 摘要」的入口。
                     */}
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
