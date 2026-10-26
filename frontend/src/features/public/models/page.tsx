/**
 * `/models` 模型库页面编排。
 *
 * URL 是唯一状态源：筛选、排序、视图、时间范围与对比集合全部来自路由，
 * 页面只把用户操作转换成一个新的 `ModelsSearch` 写回。派生数据全部由纯
 * 函数计算（`filterEntries` / `sortEntries` / `entriesOf` / `facetsOf`），
 * 组件本身不发额外请求：
 *
 * - 目录（`GET /api/pricing`，逐页拉全）决定有哪些模型；
 * - 指标（`GET /api/pricing/metrics?range=`）只影响读数，失败时降级成
 *   「全部 `—`」，列表照常完整渲染——这是模型库最重要的可用性约定。
 */

import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState, type ReactNode } from 'react'

import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { useMediaQuery } from '@/shared/lib/use-media-query'
import { MobileSheet } from '@/features/public/mobile-sheet'
import type { PublicModelCard } from '@/features/public/pricing/api'

import {
  indexMetrics,
  libraryMetricsQueryOptions,
  modelCatalogQueryOptions,
} from './api'
import { CallDialog } from './call-dialog'
import { CompareDialog } from './compare-dialog'
import { ModelFilters } from './filter-panel'
import { activeFilterCount, filterEntries, sortEntries } from './filters'
import { GlossaryDialog } from './glossary-dialog'
import { ModelHero } from './hero'
import {
  compareIds,
  MAX_COMPARE,
  validateModelsSearch,
  withCompareIds,
  type ModelsSearch,
  type ModelsView,
} from './params'
import {
  entriesOf,
  type CallKind,
  type ModelEntry,
} from './performance'
import { Register } from './register'
import { facetsOf } from './taxonomy'
import { Toolbar } from './toolbar'

export interface ModelsPageProps {
  search: ModelsSearch
  onSearchChange: (next: ModelsSearch, replace?: boolean) => void
}

/** 与演示稿 workbench 同宽：低于 1200px 时筛选栏收进抽屉。 */
const WIDE_QUERY = '(min-width: 1200px)'

/** ≤760px 表格不可读：卡片成为唯一的列表视图。 */
const NARROW_QUERY = '(max-width: 760px)'

interface CallTarget {
  card: PublicModelCard
  kind: CallKind
}

function ModelSkeleton() {
  return (
    <div className="flex flex-col gap-2" aria-hidden="true">
      {[0, 1, 2, 3, 4].map((index) => (
        <div
          key={index}
          className="h-11 rounded border border-border-hairline bg-surface-subtle"
        />
      ))}
    </div>
  )
}

export function ModelsPage({ search, onSearchChange }: ModelsPageProps) {
  const { t, locale } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const catalog = useQuery(modelCatalogQueryOptions())
  const metrics = useQuery({
    ...libraryMetricsQueryOptions(search.range),
    enabled: catalog.isSuccess,
  })
  const wide = useMediaQuery(WIDE_QUERY)
  // `useMediaQuery` treats a missing `matchMedia` (SSR, old embedded webviews)
  // as a match so the page degrades to desktop. A max-width query must not read
  // that fallback as "narrow", or those environments would be forced to cards.
  const narrow =
    useMediaQuery(NARROW_QUERY) &&
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function'

  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [filtersOpen, setFiltersOpen] = useState(false)
  const [compareOpen, setCompareOpen] = useState(false)
  const [glossaryOpen, setGlossaryOpen] = useState(false)
  const [call, setCall] = useState<CallTarget | null>(null)
  const [metricsDismissed, setMetricsDismissed] = useState(false)

  const cards = catalog.data?.items
  const facets = facetsOf(cards ?? [])
  const metricsIndex = indexMetrics(metrics.data?.items ?? [])
  /**
   * 计费元数据可能缺失（裁剪过的旧版响应）：显式判空后再取
   * `quota_per_unit`，否则整页会因读取 `undefined` 的字段而白屏。价格换算
   * 本身也要求 `quota_per_unit` 有限且为正，无效值一律退化成 `—`。
   */
  const meta = catalog.data?.meta
  const quotaPerUnit =
    meta !== undefined && Number.isFinite(meta.quota_per_unit)
      ? meta.quota_per_unit
      : undefined
  const entries = entriesOf(cards ?? [], metricsIndex, quotaPerUnit)
  // 指标不可用时每条读数都是「无数据」，`measured` 筛选会把整张列表清空。
  // 模型库的保底约定是「读数降级、列表完整」，所以此时绕过该筛选；如果
  // 只是后台重取失败但仍有旧读数，则照常按读数筛选。
  const metricsUnavailable = metrics.isError && metrics.data === undefined
  const effectiveSearch = metricsUnavailable
    ? { ...search, measured: false }
    : search
  const visible = sortEntries(
    filterEntries(entries, effectiveSearch),
    search.sort,
    locale,
  )
  const numberFormat = new Intl.NumberFormat(locale)

  /**
   * 对比集合与目录求交：目录里已经消失的 id 会让对比栏显示一个永远打不开
   * 的模型。只在目录响应变化时修正（目录被截断时不动，否则会误删尚未拉取
   * 到的合法 id），并用 `replace` 写回，避免污染历史。
   */
  const searchRef = useRef(search)
  searchRef.current = search
  const changeRef = useRef(onSearchChange)
  changeRef.current = onSearchChange
  useEffect(() => {
    const data = catalog.data
    if (data === undefined || data.truncated) return
    const known = new Set(data.items.map((item) => item.model_id))
    const current = compareIds(searchRef.current)
    const kept = current.filter((id) => known.has(id))
    if (kept.length === current.length) return
    changeRef.current(
      { ...searchRef.current, compare: withCompareIds(kept) },
      true,
    )
  }, [catalog.data])

  /**
   * 筛选变化不重置展开状态（展开按 model_id 保留）；只有模型真的从目录里
   * 消失时才清空，避免下一轮 `aria-controls` 指向不存在的详情区。
   */
  useEffect(() => {
    if (expandedId === null) return
    const items = catalog.data?.items
    if (items === undefined) return
    if (!items.some((item) => item.model_id === expandedId)) {
      setExpandedId(null)
    }
  }, [catalog.data, expandedId])

  // 换时间范围就是换一个指标窗口：上一窗口被关掉的失败提示不能顺延，
  // 否则新窗口同样失败时既看不到提示、也点不到重试。
  useEffect(() => {
    setMetricsDismissed(false)
  }, [search.range])

  // 视口变宽后筛选栏常驻，抽屉应关闭；否则缩回窄屏时它又会自己弹出来。
  useEffect(() => {
    if (wide) setFiltersOpen(false)
  }, [wide])

  const entriesById = new Map(
    entries.map((entry) => [entry.card.model_id, entry]),
  )
  const compared: ModelEntry[] = []
  for (const id of compareIds(search)) {
    const entry = entriesById.get(id)
    if (entry !== undefined) compared.push(entry)
  }

  const toggleExpand = (modelId: string) =>
    setExpandedId((current) => (current === modelId ? null : modelId))

  const toggleCompare = (modelId: string) => {
    const current = compareIds(search)
    if (current.includes(modelId)) {
      onSearchChange({
        ...search,
        compare: withCompareIds(current.filter((id) => id !== modelId)),
      })
      return
    }
    if (current.length >= MAX_COMPARE) return
    onSearchChange({
      ...search,
      compare: withCompareIds([...current, modelId]),
    })
  }

  const removeFromCompare = (modelId: string) => {
    onSearchChange({
      ...search,
      compare: withCompareIds(
        compareIds(search).filter((id) => id !== modelId),
      ),
    })
  }

  const clearCompare = () =>
    onSearchChange({ ...search, compare: withCompareIds([]) })

  const clearFilters = () =>
    onSearchChange(validateModelsSearch({ view: search.view }))

  /**
   * 窄屏下卡片是布局强制，不是用户选择：表格/卡片切换仍渲染（`aria-pressed`
   * 由 `toolbarView` 反映生效视图），但 view 变化绝不写回 URL；其它工具栏
   * 改动也必须保留地址栏里原有的 view，不能把强制的 cards 带进去。
   */
  const toolbarView: ModelsView = narrow ? 'cards' : search.view
  const handleToolbarChange = (next: ModelsSearch, replace?: boolean) => {
    if (!narrow) {
      onSearchChange(next, replace)
      return
    }
    const changedOther = (
      [
        'q',
        'cap',
        'vendor',
        'context',
        'endpoint',
        'measured',
        'sort',
        'range',
        'compare',
      ] as const
    ).some((key) => next[key] !== search[key])
    if (!changedOther) return
    onSearchChange({ ...next, view: search.view }, replace)
  }

  let list: ReactNode
  if (catalog.isError) {
    list = (
      <div
        role="alert"
        className="rounded-xl border border-border-hairline bg-surface-card px-4 py-8 text-center"
      >
        <p className="text-body text-ink-1">{t('public.models.load_failed')}</p>
        <button
          type="button"
          onClick={() => void catalog.refetch()}
          className="mt-4 rounded-lg border border-border-default px-4 py-2 text-body font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.models.retry')}
        </button>
      </div>
    )
  } else if (catalog.isPending) {
    list = (
      <div role="status" className="flex flex-col gap-3">
        <p className="text-meta text-ink-3">{t('public.models.loading')}</p>
        <ModelSkeleton />
      </div>
    )
  } else if (catalog.data !== undefined) {
    const catalogData = catalog.data
    list = (
      <>
        {catalogData.truncated ? (
          <p
            role="status"
            className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-meta text-ink-1"
          >
            {t('public.models.truncated', {
              count: numberFormat.format(catalogData.items.length),
              total: numberFormat.format(catalogData.total),
            })}
          </p>
        ) : null}

        {metrics.isError && !metricsDismissed ? (
          <div
            role="alert"
            className="flex flex-wrap items-center gap-3 rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-meta text-ink-1"
          >
            <span className="min-w-0 flex-1">
              {t('public.models.metrics_unavailable')}
            </span>
            <button
              type="button"
              onClick={() => {
                setMetricsDismissed(false)
                void metrics.refetch()
              }}
              className="rounded-md border border-border-default px-2 py-0.5 text-badge font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.models.retry')}
            </button>
            <button
              type="button"
              aria-label={t('public.models.metrics_dismiss')}
              onClick={() => setMetricsDismissed(true)}
              className="rounded p-0.5 leading-none text-ink-2 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              <span aria-hidden="true">×</span>
            </button>
          </div>
        ) : null}

        {visible.length === 0 ? (
          <div className="rounded-xl border border-border-hairline bg-surface-card px-4 py-10 text-center">
            <h2 className="text-card-title text-ink-1">
              {t('public.models.register.empty')}
            </h2>
            <p className="mt-1 text-body text-ink-2">
              {t('public.models.register.empty_hint')}
            </p>
            <button
              type="button"
              onClick={clearFilters}
              className="mt-4 rounded-lg border border-border-default px-4 py-2 text-body font-medium text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.models.register.clear')}
            </button>
          </div>
        ) : (
          <Register
            entries={visible}
            view={search.view}
            narrow={narrow}
            range={search.range}
            compareIds={compareIds(search)}
            expandedId={expandedId}
            onToggleExpand={toggleExpand}
            onToggleCompare={toggleCompare}
            onOpenCompare={() => setCompareOpen(true)}
            onCall={(entry) => setCall({ card: entry.card, kind: entry.kind })}
          />
        )}
      </>
    )
  } else {
    list = null
  }

  const filterPanel = (
    <ModelFilters
      facets={facets}
      search={search}
      onChange={onSearchChange}
      onClose={wide ? undefined : () => setFiltersOpen(false)}
    />
  )

  return (
    <div className="flex flex-col gap-6">
      <ModelHero entries={entries} />

      <div>
        <button
          type="button"
          onClick={() => setGlossaryOpen(true)}
          className="rounded text-meta text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.models.glossary.title')}
        </button>
      </div>

      <div
        className={
          wide
            ? 'grid grid-cols-[220px_minmax(0,1fr)] items-start gap-10'
            : 'flex min-w-0 flex-col gap-3'
        }
      >
        {wide ? (
          <aside
            aria-label={t('public.models.filters.title')}
            className="sticky top-8 min-w-0"
          >
            {filterPanel}
          </aside>
        ) : null}

        <div className="flex min-w-0 flex-col gap-3">
          <Toolbar
            search={
              toolbarView === search.view
                ? search
                : { ...search, view: toolbarView }
            }
            resultCount={visible.length}
            activeFilterCount={activeFilterCount(search)}
            narrow={narrow}
            onChange={handleToolbarChange}
            onOpenFilters={wide ? undefined : () => setFiltersOpen(true)}
          />
          {list}
        </div>
      </div>

      <p className="border-t border-border-hairline pt-3 text-badge text-ink-3">
        {t('public.models.footer.window', {
          window: t(
            search.range === '24h'
              ? 'public.models.detail.window_24h'
              : 'public.models.detail.window_7d',
          ),
        })}
      </p>

      <MobileSheet
        open={filtersOpen && !wide}
        onOpenChange={setFiltersOpen}
        title={t('public.models.filters.title')}
        closeLabel={t('public.models.filters.close')}
      >
        <div className="max-h-[65vh] overflow-y-auto">{filterPanel}</div>
      </MobileSheet>

      <CompareDialog
        open={compareOpen}
        onOpenChange={setCompareOpen}
        entries={compared}
        onRemove={removeFromCompare}
        onClear={clearCompare}
      />

      <GlossaryDialog open={glossaryOpen} onOpenChange={setGlossaryOpen} />

      {call === null ? null : (
        <CallDialog
          open
          onOpenChange={(next) => {
            if (!next) setCall(null)
          }}
          model={call.card}
          kind={call.kind}
          serverAddress={status.data?.site?.server_address}
          isLoggedIn={me.isSuccess}
        />
      )}
    </div>
  )
}
