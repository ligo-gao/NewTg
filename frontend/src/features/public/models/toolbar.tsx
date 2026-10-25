/**
 * 模型库工具条：搜索、结果数、测量窗口、排序、视图与筛选入口。
 *
 * 工具条不持有筛选状态：每次写回都把改动合并进传入的 `search`，其余键
 * （包括 `compare`、`sort`、`range`、`view`）原样保留，避免切一个参数时
 * 悄悄清掉另一个。搜索框只在输入时持有草稿，提交/清空才写 URL；这是
 * 「地址栏是唯一状态源」的唯一例外。
 */

import { useEffect, useRef, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import { SEARCH_MAX_LENGTH } from '@/features/public/pricing/params'

import type { ModelsRange, ModelsSearch, ModelsSort, ModelsView } from './params'

export interface ToolbarProps {
  search: ModelsSearch
  resultCount: number
  activeFilterCount: number
  onChange: (next: ModelsSearch, replace?: boolean) => void
  /**
   * 只在筛选栏能打开时传入（窄屏抽屉）。宽屏筛选栏常驻，按钮会成为一个
   * 点了没反应的死控件，因此没有这个回调就不渲染入口。
   */
  onOpenFilters?: () => void
}

const RANGES: readonly { value: ModelsRange; label: string }[] = [
  { value: '24h', label: 'public.perf.range.24h' },
  { value: '7d', label: 'public.perf.range.7d' },
]

const SORTS: readonly ModelsSort[] = [
  'measured',
  'name',
  'latency',
  'tps',
  'success',
  'context',
]

const VIEWS: readonly ModelsView[] = ['table', 'cards']

export function Toolbar({
  search,
  resultCount,
  activeFilterCount,
  onChange,
  onOpenFilters,
}: ToolbarProps) {
  const { t, locale } = useI18n()
  const [draft, setDraft] = useState(search.q)
  const lastCommitted = useRef(search.q)

  // 地址栏被外部改变（后退、清除筛选、别人分享的链接）时同步草稿，但不
  // 覆盖用户正在输入、尚未提交的文本。
  useEffect(() => {
    if (search.q !== lastCommitted.current) {
      lastCommitted.current = search.q
      setDraft(search.q)
    }
  }, [search.q])

  const commit = (value: string) => {
    lastCommitted.current = value
    setDraft(value)
    onChange({ ...search, q: value })
  }

  return (
    <div className="sticky top-0 z-20 flex flex-wrap items-center gap-2 border-b border-border-hairline bg-surface-card py-2">
      <form
        role="search"
        aria-label={t('public.models.search.label')}
        onSubmit={(event) => {
          event.preventDefault()
          const trimmed = draft.trim()
          commit(trimmed)
          setDraft(trimmed)
        }}
        className="relative w-full md:w-[320px]"
      >
        <span
          aria-hidden="true"
          className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-ink-3"
        >
          <svg
            viewBox="0 0 16 16"
            className="size-4"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          >
            <circle cx="7" cy="7" r="4.5" />
            <path d="m10.5 10.5 3 3" strokeLinecap="round" />
          </svg>
        </span>
        <input
          type="search"
          value={draft}
          maxLength={SEARCH_MAX_LENGTH}
          onChange={(event) =>
            setDraft(event.target.value.slice(0, SEARCH_MAX_LENGTH))
          }
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              event.preventDefault()
              commit('')
            }
          }}
          placeholder={t('public.models.search.placeholder')}
          aria-label={t('public.models.search.label')}
          className="h-10 w-full rounded-lg border border-border-default bg-surface-card pl-9 pr-9 text-body text-ink-1 placeholder:text-ink-3 focus:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        />
        {draft === '' ? null : (
          <button
            type="button"
            aria-label={t('public.models.search.clear')}
            onClick={() => commit('')}
            className="absolute right-2 top-1/2 inline-flex size-6 -translate-y-1/2 items-center justify-center rounded text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            <svg
              aria-hidden="true"
              viewBox="0 0 16 16"
              className="size-4"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
            >
              <path d="m4 4 8 8M12 4l-8 8" strokeLinecap="round" />
            </svg>
          </button>
        )}
      </form>

      <p className="text-meta leading-5 tabular-nums text-ink-2" aria-live="polite">
        {t('public.models.result_count', {
          count: new Intl.NumberFormat(locale).format(resultCount),
        })}
      </p>

      <div className="ml-auto flex flex-wrap items-center gap-2">
        <div
          role="group"
          aria-label={t('public.models.range_label')}
          className="inline-flex items-center gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-1"
        >
          {RANGES.map((option) => {
            const selected = search.range === option.value
            return (
              <button
                key={option.value}
                type="button"
                aria-pressed={selected}
                onClick={() => onChange({ ...search, range: option.value })}
                className={[
                  'rounded-md px-2.5 py-0.5 text-meta font-medium leading-5 transition-colors duration-150 motion-reduce:transition-none',
                  selected
                    ? 'bg-surface-card text-ink-1 shadow-sm'
                    : 'text-ink-2 hover:text-ink-1',
                  'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                ].join(' ')}
              >
                {t(option.label)}
              </button>
            )
          })}
        </div>

        <select
          value={search.sort}
          onChange={(event) =>
            onChange({ ...search, sort: event.target.value as ModelsSort })
          }
          aria-label={t('public.models.sort.label')}
          className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-meta text-ink-1 focus:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {SORTS.map((sort) => (
            <option key={sort} value={sort}>
              {t(`public.models.sort.${sort}`)}
            </option>
          ))}
        </select>

        <div
          role="group"
          aria-label={t('public.models.view.label')}
          className="inline-flex items-center gap-1 rounded-lg border border-border-hairline bg-surface-subtle p-1"
        >
          {VIEWS.map((view) => {
            const selected = search.view === view
            return (
              <button
                key={view}
                type="button"
                aria-pressed={selected}
                onClick={() => onChange({ ...search, view })}
                className={[
                  'rounded-md px-2.5 py-0.5 text-meta font-medium leading-5 transition-colors duration-150 motion-reduce:transition-none',
                  selected
                    ? 'bg-surface-card text-ink-1 shadow-sm'
                    : 'text-ink-2 hover:text-ink-1',
                  'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                ].join(' ')}
              >
                {t(`public.models.view.${view}`)}
              </button>
            )
          })}
        </div>

        {onOpenFilters === undefined ? null : (
          <button
            type="button"
            onClick={onOpenFilters}
            className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-border-default bg-surface-card px-3 text-meta font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            <svg
              aria-hidden="true"
              viewBox="0 0 16 16"
              className="size-4"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
            >
              <path d="M2 4h12M4 8h8M6 12h4" strokeLinecap="round" />
            </svg>
            {activeFilterCount > 0
              ? t('public.models.filter_button_count', {
                  count: activeFilterCount,
                })
              : t('public.models.filter_button')}
          </button>
        )}
      </div>
    </div>
  )
}
