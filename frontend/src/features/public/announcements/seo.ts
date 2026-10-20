/**
 * 公开公告页的 head 管理（F-08-04 公开详情）。
 *
 * 与首页 `PublicSeo` 同样的约束：SPA 没有 SSR，页面挂载后写 head；卸载时
 * 必须把标题/描述恢复成进入本页之前的值，否则客户端路由切到关于页时仍会
 * 残留上一则公告的标题。
 *
 * 标题观察器：i18n provider 在自己的语言切换 effect 里重写 `document.title`
 * （web-00 §7 TC-C-02），而它的 effect 可能晚于本页 effect 执行。观察
 * `<title>` 并在被改写时重新写入公告标题，保证本页挂载期间标题属于本页；
 * 卸载时断开观察并恢复快照。
 */

import { useEffect } from 'react'

function upsertMeta(name: string, content: string | null) {
  const head = document.head
  const existing = head.querySelector<HTMLMetaElement>(`meta[name="${name}"]`)
  if (content === null || content === '') {
    existing?.remove()
    return
  }
  if (existing !== null) {
    existing.setAttribute('content', content)
    return
  }
  const element = document.createElement('meta')
  element.setAttribute('name', name)
  element.setAttribute('content', content)
  head.appendChild(element)
}

function readMeta(name: string): string | null {
  return (
    document.head
      .querySelector<HTMLMetaElement>(`meta[name="${name}"]`)
      ?.getAttribute('content') ?? null
  )
}

/**
 * `title` / `description` 为 `null` 表示数据尚未就绪：不改动 head，
 * 避免公告尚未加载时先把标题刷成占位文案。
 */
export function useAnnouncementSeo(
  title: string | null,
  description: string | null,
): void {
  useEffect(() => {
    if (title === null) return
    const previousTitle = document.title
    const previousDescription = readMeta('description')

    document.title = title
    upsertMeta('description', description)

    const titleElement = document.head.querySelector('title')
    const observer =
      titleElement === null
        ? null
        : new MutationObserver(() => {
            if (document.title !== title) document.title = title
          })
    if (titleElement !== null) {
      observer?.observe(titleElement, {
        childList: true,
        characterData: true,
        subtree: true,
      })
    }

    return () => {
      observer?.disconnect()
      document.title = previousTitle
      upsertMeta('description', previousDescription)
    }
  }, [title, description])
}
