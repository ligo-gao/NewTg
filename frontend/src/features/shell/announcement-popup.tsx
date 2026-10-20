import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'

import {
  listActiveAnnouncements,
  markAnnouncementRead,
  type ActiveAnnouncement,
} from '@/features/announcements/api'
import { ANNOUNCEMENT_ACTIVE_QUERY_KEY } from '@/features/announcements/query-keys'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { Tag } from '@/shared/ui/tag'

/**
 * 「同一会话内不重复弹」的本地记忆。服务端已读记录才是跨会话的事实，
 * 这里只覆盖「确认后标记已读请求失败 / 尚未落库」的窗口，避免同一
 * 公告在一次浏览会话里反复出现。sessionStorage 在隐私模式下可能不可用，
 * 读写都失败关闭（退化为仅本组件生命周期内去重，仍不阻塞页面）。
 */
const DISMISSED_SESSION_KEY = 'newtg.announcements.dismissed'

function readDismissed(): string[] {
  try {
    const raw = globalThis.sessionStorage?.getItem(DISMISSED_SESSION_KEY)
    if (raw === null || raw === undefined) return []
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed)
      ? parsed.filter((item): item is string => typeof item === 'string')
      : []
  } catch {
    return []
  }
}

function writeDismissed(ids: ReadonlySet<string>): void {
  try {
    globalThis.sessionStorage?.setItem(DISMISSED_SESSION_KEY, JSON.stringify([...ids]))
  } catch {
    // 本地记忆不可用不影响确认动作本身。
  }
}

/**
 * 用户侧公告弹层（modules-08 §6.1.1；组件归属 modules-01 控制台外壳）。
 *
 * 挂在 `_console` 布局层：登录后拉一次生效中的公告，对未读项显示一次性
 * `role=alertdialog`。它不在路由 loader 里，也不阻塞任何导航；无公告或
 * 请求失败时渲染为空，且 `retry: false` 保证不会形成重试风暴。
 */
export function AnnouncementPopup() {
  const { t } = useI18n()
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(
    () => new Set(readDismissed()),
  )

  const query = useQuery({
    queryKey: ANNOUNCEMENT_ACTIVE_QUERY_KEY,
    queryFn: ({ signal }) => listActiveAnnouncements(signal),
    // 公告不是实时流：一次会话拉一次即可。服务端失败也不重试。
    staleTime: 5 * 60_000,
    retry: false,
  })

  const markRead = useMutation({
    mutationFn: (id: string) => markAnnouncementRead(id),
    retry: false,
  })

  const current: ActiveAnnouncement | undefined = query.data?.items.find(
    (item) => !item.read && !dismissed.has(item.id),
  )

  if (current === undefined) return null

  function acknowledge(item: ActiveAnnouncement): void {
    // 先本地出队再发请求：网络失败时用户仍能继续使用控制台；服务端未读到
    // 记录会在下一次会话重新弹出，这是可接受且符合「一次性」语义的降级。
    setDismissed((previous) => {
      const next = new Set(previous)
      next.add(item.id)
      writeDismissed(next)
      return next
    })
    markRead.mutate(item.id)
  }

  return (
    <Dialog
      open
      onOpenChange={() => {
        // 公告要求用户明确确认：Escape / 遮罩点击不得吞掉未读公告。
      }}
      dismissible={false}
      role="alertdialog"
      title={current.title}
      footer={
        <Button onClick={() => acknowledge(current)}>
          {t('common.announcement_popup.confirm')}
        </Button>
      }
    >
      <div className="flex flex-col gap-3">
        <Tag ariaLabel={t('common.announcement_popup.category_label')}>
          {t(`common.announcement_popup.category_${current.category}`)}
        </Tag>
        {/*
         * content_html 来自服务端 `markup.MarkdownToHTML`：原始 HTML 不解析，
         * 链接仅 http(s) 且强制 rel=noopener noreferrer（R-D7-04）。
         * 前端不再二次解析 Markdown，也不把 content_markdown 当 HTML 注入。
         */}
        <div
          data-testid="announcement-popup-content"
          className="break-words text-sm leading-6 text-ink-1 [&_a]:text-brand [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:border-border-default [&_blockquote]:pl-3 [&_code]:rounded [&_code]:bg-surface-subtle [&_code]:px-1 [&_h1]:text-lg [&_h1]:font-semibold [&_h2]:text-base [&_h2]:font-semibold [&_li]:ml-5 [&_li]:list-disc [&_ol_li]:list-decimal [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-surface-subtle [&_pre]:p-2"
          dangerouslySetInnerHTML={{ __html: current.content_html }}
        />
      </div>
    </Dialog>
  )
}
