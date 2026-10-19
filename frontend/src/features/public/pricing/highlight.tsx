import type { ReactNode } from 'react'

/**
 * 搜索命中高亮（F-09-16）。只做大小写不敏感的子串标记，不引入正则语法：
 * 用户可以输入 `(` 或 `*`，把它们当正则会把页面打成空白。
 */
export function HighlightedText({
  text,
  query,
}: {
  text: string
  query: string
}): ReactNode {
  const needle = query.trim()
  if (needle === '') return text
  const haystack = text.toLowerCase()
  const lower = needle.toLowerCase()
  const parts: ReactNode[] = []
  let cursor = 0
  let index = haystack.indexOf(lower)
  let key = 0
  while (index !== -1) {
    if (index > cursor) parts.push(text.slice(cursor, index))
    parts.push(
      <mark
        key={`hit-${key}`}
        className="rounded-sm bg-brand-subtle text-ink-1"
      >
        {text.slice(index, index + needle.length)}
      </mark>,
    )
    key += 1
    cursor = index + needle.length
    index = haystack.indexOf(lower, cursor)
  }
  if (cursor < text.length) parts.push(text.slice(cursor))
  return <>{parts}</>
}
