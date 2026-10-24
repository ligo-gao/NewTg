/**
 * `/models` 模型库页面（Task 12 替换实现）。本任务只需要一个可编译的占位，
 * 路由壳已经按最终的 `search` / `onSearchChange` 契约挂载它。
 */

import type { ModelsSearch } from './params'

export interface ModelsPageProps {
  search: ModelsSearch
  onSearchChange: (next: ModelsSearch, replace?: boolean) => void
}

export function ModelsPage({ search }: ModelsPageProps) {
  return (
    <section className="flex flex-col gap-3">
      <h1 className="text-page-title font-semibold text-ink-1">Model Library</h1>
      <p className="text-body text-ink-2">
        Browse every model this gateway exposes, compare measured performance,
        and copy integration snippets.
      </p>
      {search.q !== '' ? (
        <p className="text-body text-ink-2">Searching for “{search.q}”.</p>
      ) : null}
    </section>
  )
}
